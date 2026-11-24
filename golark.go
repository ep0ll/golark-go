// Package golark embeds a statically typed Go-like configuration language.
//
// Golark reuses Go's syntax tree, parser, formatter, and type checker. A
// checked package graph is executed by Golark's evaluator, allowing
// applications to provide imported source packages and typed native bindings.
package golark

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"

	"github.com/ep0ll/golark-go/internal/checker"
	"github.com/ep0ll/golark-go/internal/eval"
	"github.com/ep0ll/golark-go/internal/loader"
	"github.com/ep0ll/golark-go/internal/syntax"
)

// Binding pairs an application value with the static type visible in scripts.
// Use Bind for ordinary Go values, or set Type explicitly for custom types.
type Binding = checker.Binding

// Bind creates a statically typed binding from a Go value.
func Bind(value any) Binding { return checker.Bind(value) }

// PackageSource is the in-memory source for one imported Go package.
type PackageSource = loader.PackageSource

// PackageResolver resolves import paths to source packages. It is invoked by
// one Exec operation for every source-backed direct or transitive import.
type PackageResolver = loader.PackageResolver

// ResolverFunc adapts a function to PackageResolver.
type ResolverFunc = loader.ResolverFunc

// ErrPackageNotFound asks Golark to try Options.Importer for an import path.
var ErrPackageNotFound = loader.ErrPackageNotFound

// Options configures parsing, type checking, package resolution, and execution.
type Options struct {
	// PackagePath is the import path assigned to the root script package. When
	// empty, Golark uses golark.local/<package-name>.
	PackagePath string
	// Resolver supplies in-memory Go source for imported packages. The same
	// resolver is used for transitive imports, and each import path is resolved
	// at most once during one Check or Exec operation.
	Resolver PackageResolver
	// Importer supplies type information for native/type-only packages. Source
	// packages returned by Resolver take precedence. If nil, unresolved imports
	// fail rather than being discovered through Go modules or the filesystem.
	Importer types.Importer
	// Predeclared exposes host values as package-level identifiers in the root
	// script package.
	Predeclared map[string]Binding
	// Imports supplies runtime values for native packages, keyed by exact
	// import path and exported identifier. Static types come from Importer.
	Imports map[string]map[string]Binding
	// MaxSteps bounds interpreted execution. Zero selects the default budget.
	MaxSteps int64
}

// Program is a parsed Go package. Its syntax is standard Go syntax.
type Program struct{ parsed *checker.Program }

// CheckedProgram is a Program and its complete, type-checked source package graph.
type CheckedProgram struct {
	checked *checker.Checked
	graph   *loader.Graph
}

// Module is an initialized script package.
type Module struct{ module *eval.Module }

// LexToken is one token produced by the Go standard-library scanner.
type LexToken = syntax.Token

// Lex tokenizes source using go/scanner and returns source positions.
func Lex(filename string, source []byte) ([]LexToken, error) {
	return syntax.Lex(filename, source)
}

// ParseExpr parses a single Go expression using go/parser.
func ParseExpr(filename string, source []byte) (ast.Expr, error) {
	return syntax.ParseExpr(token.NewFileSet(), filename, source)
}

// FormatSource formats a Go source fragment using go/format.
func FormatSource(filename string, source []byte) ([]byte, error) {
	return syntax.FormatSource(filename, source)
}

// Parse parses one Go source file using go/parser.
func Parse(filename string, source []byte) (*Program, error) {
	fset := token.NewFileSet()
	file, err := syntax.ParseFile(fset, filename, source)
	if err != nil {
		return nil, err
	}
	return &Program{parsed: &checker.Program{Fset: fset, Files: []*ast.File{file}}}, nil
}

// ParseFiles parses multiple Go source files as one package. The map keys are
// filenames; files are sorted to make diagnostics and initialization order
// deterministic.
func ParseFiles(files map[string][]byte) (*Program, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("golark: no source files")
	}
	fset := token.NewFileSet()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sortStrings(names)
	parsed := make([]*ast.File, 0, len(names))
	for _, name := range names {
		file, err := syntax.ParseFile(fset, name, files[name])
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, file)
	}
	return &Program{parsed: &checker.Program{Fset: fset, Files: parsed}}, nil
}

// Check performs Go static type checking for the root package and every
// source-backed direct or transitive import. All packages share one importer
// cache and token.FileSet.
func Check(program *Program, options Options) (*CheckedProgram, error) {
	return check(context.Background(), program, options)
}

func check(ctx context.Context, program *Program, options Options) (*CheckedProgram, error) {
	if program == nil {
		return nil, fmt.Errorf("golark: nil program")
	}
	graph, err := loader.Load(ctx, program.parsed, loader.Options{
		PackagePath: options.PackagePath,
		Resolver:    options.Resolver,
		Importer:    options.Importer,
		Predeclared: options.Predeclared,
	})
	if err != nil {
		return nil, err
	}
	return &CheckedProgram{checked: graph.Root(), graph: graph}, nil
}

// Exec parses, type-checks the complete package graph, initializes imported
// source packages in dependency order, then initializes the root package.
func Exec(ctx context.Context, filename string, source []byte, options Options) (*Module, error) {
	program, err := Parse(filename, source)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, program, options)
}

// Execute type-checks and initializes a previously parsed package and its
// complete source-backed import graph.
func Execute(ctx context.Context, program *Program, options Options) (*Module, error) {
	checked, err := check(ctx, program, options)
	if err != nil {
		return nil, err
	}

	imports := cloneImports(options.Imports)
	var root *eval.Module
	for _, pkg := range checked.graph.Packages() {
		predeclared := map[string]checker.Binding(nil)
		if pkg == checked.checked {
			predeclared = options.Predeclared
		}
		runtime, err := eval.New(pkg, eval.Options{
			Predeclared: predeclared,
			Imports:     imports,
			MaxSteps:    options.MaxSteps,
		})
		if err != nil {
			return nil, err
		}
		module, err := runtime.Init(ctx)
		if err != nil {
			return nil, fmt.Errorf("initialize package %q: %w", pkg.Package.Path(), err)
		}
		imports[pkg.Package.Path()] = runtimeBindings(pkg, module)
		if pkg == checked.checked {
			root = module
		}
	}
	if root == nil {
		return nil, fmt.Errorf("golark: package graph did not initialize its root")
	}
	return &Module{module: root}, nil
}

func cloneImports(source map[string]map[string]checker.Binding) map[string]map[string]checker.Binding {
	out := make(map[string]map[string]checker.Binding, len(source))
	for path, bindings := range source {
		copyBindings := make(map[string]checker.Binding, len(bindings))
		for name, binding := range bindings {
			copyBindings[name] = binding
		}
		out[path] = copyBindings
	}
	return out
}

// runtimeBindings exposes only exported package-level values. Named types
// remain represented by the exact *types.Package graph used during checking.
func runtimeBindings(checked *checker.Checked, module *eval.Module) map[string]checker.Binding {
	out := make(map[string]checker.Binding)
	for _, name := range checked.Package.Scope().Names() {
		if !token.IsExported(name) {
			continue
		}
		object := checked.Package.Scope().Lookup(name)
		switch object.(type) {
		case *types.Const, *types.Func, *types.Var:
			value, ok := module.Lookup(name)
			if ok {
				out[name] = checker.Binding{Value: value, Type: object.Type()}
			}
		}
	}
	return out
}

// ExecFile reads, parses, checks, and initializes a source file.
func ExecFile(ctx context.Context, filename string, options Options) (*Module, error) {
	source, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return Exec(ctx, filename, source, options)
}

// Globals returns a shallow copy of the module's exported global values.
func (m *Module) Globals() map[string]any {
	if m == nil || m.module == nil {
		return nil
	}
	return m.module.Globals()
}

// Lookup retrieves a module global by name.
func (m *Module) Lookup(name string) (any, bool) {
	if m == nil || m.module == nil {
		return nil, false
	}
	return m.module.Lookup(name)
}

// Call invokes a top-level script function by name.
func (m *Module) Call(ctx context.Context, name string, args ...any) ([]any, error) {
	if m == nil || m.module == nil {
		return nil, fmt.Errorf("golark: nil module")
	}
	return m.module.Call(ctx, name, args...)
}

// CallValue invokes a callable global or a host function obtained from Lookup.
func (m *Module) CallValue(ctx context.Context, callable any, args ...any) ([]any, error) {
	if m == nil || m.module == nil {
		return nil, fmt.Errorf("golark: nil module")
	}
	return m.module.CallValue(ctx, callable, args...)
}

// AST returns the parsed files. The returned nodes are owned by Program and
// should be treated as immutable after checking.
func (p *Program) AST() []*ast.File {
	if p == nil || p.parsed == nil {
		return nil
	}
	return append([]*ast.File(nil), p.parsed.Files...)
}

// Types returns the checked root package and its type information.
func (p *CheckedProgram) Types() (*types.Package, *types.Info) {
	if p == nil || p.checked == nil {
		return nil, nil
	}
	return p.checked.Package, p.checked.Info
}

// Packages returns source-backed packages in dependency-first order, including
// the root. Every import path in the graph has one stable type identity.
func (p *CheckedProgram) Packages() []*types.Package {
	if p == nil || p.graph == nil {
		return nil
	}
	checked := p.graph.Packages()
	out := make([]*types.Package, 0, len(checked))
	for _, pkg := range checked {
		out = append(out, pkg.Package)
	}
	return out
}

// Format formats a parsed program using go/format.
func (p *Program) Format() ([]byte, error) {
	if p == nil || p.parsed == nil {
		return nil, fmt.Errorf("golark: nil program")
	}
	var out []byte
	for i, file := range p.parsed.Files {
		b, err := syntax.Format(p.parsed.Fset, file)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, b...)
	}
	return out, nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
