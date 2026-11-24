// Package golark embeds a statically typed Go-like configuration language.
//
// Golark deliberately reuses Go's own syntax tree, parser, formatter, and type
// checker. A checked program is then executed by Golark's evaluator, allowing
// applications to expose typed Go functions and values to scripts.
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
	"github.com/ep0ll/golark-go/internal/syntax"
)

// Binding pairs an application value with the static type visible in scripts.
// Use Bind for ordinary Go values, or set Type explicitly for custom types.
type Binding = checker.Binding

// Bind creates a statically typed binding from a Go value.
func Bind(value any) Binding { return checker.Bind(value) }

// Options configures parsing, checking, and execution.
type Options struct {
	// PackagePath is the import path assigned to the script package. When empty,
	// Golark uses golark.local/<package-name>.
	PackagePath string
	// Importer resolves packages referenced by Go import declarations. The
	// default is go/importer.Default, which resolves installed Go packages.
	Importer types.Importer
	// Predeclared exposes host values as package-level identifiers.
	Predeclared map[string]Binding
	// Imports supplies runtime values for packages imported by the source,
	// keyed by the exact import path and then exported identifier.
	Imports map[string]map[string]Binding
	// MaxSteps bounds interpreted execution. Zero selects the default budget.
	MaxSteps int64
}

// Program is a parsed Go package. Its syntax is standard Go syntax.
type Program struct{ parsed *checker.Program }

// CheckedProgram is a Program that has passed Go's static type checker.
type CheckedProgram struct{ checked *checker.Checked }

// Module is an initialized script package.
type Module struct{ module *eval.Module }

// LexToken is one token produced by the Go standard-library scanner.
type LexToken = syntax.Token

// Lex tokenizes source using go/scanner and returns source positions.
func Lex(filename string, source []byte) ([]LexToken, error) { return syntax.Lex(filename, source) }

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

// Check performs Go static type checking with go/types.
func Check(program *Program, options Options) (*CheckedProgram, error) {
	if program == nil {
		return nil, fmt.Errorf("golark: nil program")
	}
	checked, err := checker.Check(program.parsed, checker.Options{PackagePath: options.PackagePath, Importer: options.Importer, Predeclared: options.Predeclared})
	if err != nil {
		return nil, err
	}
	return &CheckedProgram{checked: checked}, nil
}

// Exec parses, type-checks, and initializes one source file.
func Exec(ctx context.Context, filename string, source []byte, options Options) (*Module, error) {
	program, err := Parse(filename, source)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, program, options)
}

// Execute type-checks and initializes a previously parsed package.
func Execute(ctx context.Context, program *Program, options Options) (*Module, error) {
	checked, err := Check(program, options)
	if err != nil {
		return nil, err
	}
	runtime, err := eval.New(checked.checked, eval.Options{Predeclared: options.Predeclared, Imports: options.Imports, MaxSteps: options.MaxSteps})
	if err != nil {
		return nil, err
	}
	module, err := runtime.Init(ctx)
	if err != nil {
		return nil, err
	}
	return &Module{module: module}, nil
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

// Types returns the checked package and type information.
func (p *CheckedProgram) Types() (*types.Package, *types.Info) {
	if p == nil || p.checked == nil {
		return nil, nil
	}
	return p.checked.Package, p.checked.Info
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
