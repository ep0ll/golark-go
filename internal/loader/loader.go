// Package loader resolves and type-checks a complete in-memory Go package graph.
package loader

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/ep0ll/golark-go/internal/checker"
	"github.com/ep0ll/golark-go/internal/syntax"
)

// ErrPackageNotFound tells the loader that a resolver does not own an import
// path and that the native types.Importer may be used as a fallback.
var ErrPackageNotFound = errors.New("package not found")

// PackageSource contains the Go files for one import path. ImportPath may be
// empty, in which case the requested import path is used. Filenames are labels;
// Golark never reads them from disk.
type PackageSource struct {
	ImportPath string
	Files      map[string][]byte
}

// PackageResolver supplies source for imports. It is called once per canonical
// import path per Load operation, including for transitive imports.
type PackageResolver interface {
	Resolve(context.Context, string) (PackageSource, error)
}

// ResolverFunc adapts a function to PackageResolver.
type ResolverFunc func(context.Context, string) (PackageSource, error)

func (f ResolverFunc) Resolve(ctx context.Context, importPath string) (PackageSource, error) {
	return f(ctx, importPath)
}

// Graph is a type-checked package graph in dependency-first initialization
// order. Every occurrence of an import path in the graph refers to the same
// *types.Package instance.
type Graph struct {
	root     *checker.Checked
	packages []*checker.Checked
}

// Root returns the checked root package.
func (g *Graph) Root() *checker.Checked {
	if g == nil {
		return nil
	}
	return g.root
}

// Packages returns all source-backed packages in dependency-first order,
// including the root package. Native imported packages are represented only
// by their *types.Package and are not included here.
func (g *Graph) Packages() []*checker.Checked {
	if g == nil {
		return nil
	}
	return append([]*checker.Checked(nil), g.packages...)
}

// Options controls package-graph loading.
type Options struct {
	PackagePath string
	Resolver    PackageResolver
	Importer    types.Importer
	Predeclared map[string]checker.Binding
}

// Load checks root and every source-backed dependency in one shared import
// graph. The resolver is consulted before the optional native importer.
func Load(ctx context.Context, root *checker.Program, opts Options) (*Graph, error) {
	if root == nil || root.Fset == nil || len(root.Files) == 0 {
		return nil, fmt.Errorf("golark: cannot load an empty root package")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	path := opts.PackagePath
	if path == "" {
		path = "golark.local/" + root.Files[0].Name.Name
	}

	l := &packageLoader{
		ctx:      ctx,
		fset:     root.Fset,
		resolver: opts.Resolver,
		fallback: opts.Importer,
		states:   make(map[string]*packageState),
		native:   make(map[string]*types.Package),
	}

	state := &packageState{loading: true}
	l.states[path] = state
	checked, err := checker.Check(root, checker.Options{
		PackagePath: path,
		Importer:    l,
		Predeclared: opts.Predeclared,
	})
	if err != nil {
		state.loading = false
		state.err = err
		return nil, err
	}
	state.loading = false
	state.checked = checked
	l.ordered = append(l.ordered, checked)

	return &Graph{root: checked, packages: append([]*checker.Checked(nil), l.ordered...)}, nil
}

type packageState struct {
	loading bool
	checked *checker.Checked
	err     error
}

type packageLoader struct {
	ctx      context.Context
	fset     *token.FileSet
	resolver PackageResolver
	fallback types.Importer
	states   map[string]*packageState
	native   map[string]*types.Package
	stack    []string
	ordered  []*checker.Checked
}

// Import implements types.Importer.
func (l *packageLoader) Import(path string) (*types.Package, error) {
	return l.importPackage(path, "", 0)
}

// ImportFrom implements types.ImporterFrom. Golark keys packages by import
// path, not by source directory: source resolution is owned by the embedding
// application and package identity is canonical within this Load operation.
func (l *packageLoader) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	return l.importPackage(path, srcDir, mode)
}

func (l *packageLoader) importPackage(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	if state, ok := l.states[path]; ok {
		if state.loading {
			return nil, l.importCycle(path)
		}
		if state.err != nil {
			return nil, state.err
		}
		if state.checked != nil {
			return state.checked.Package, nil
		}
	}
	if pkg, ok := l.native[path]; ok {
		return pkg, nil
	}

	if l.resolver != nil {
		state := &packageState{loading: true}
		l.states[path] = state
		l.stack = append(l.stack, path)

		source, err := l.resolver.Resolve(l.ctx, path)
		if err == nil {
			actualPath := source.ImportPath
			if actualPath == "" {
				actualPath = path
			}
			if actualPath != path {
				err = fmt.Errorf("resolver returned import path %q for request %q", actualPath, path)
			} else {
				var checked *checker.Checked
				checked, err = l.checkSource(path, source.Files)
				if err == nil {
					state.checked = checked
					state.loading = false
					l.stack = l.stack[:len(l.stack)-1]
					l.ordered = append(l.ordered, checked)
					return checked.Package, nil
				}
			}
		}

		state.loading = false
		state.err = err
		l.stack = l.stack[:len(l.stack)-1]
		if !errors.Is(err, ErrPackageNotFound) {
			return nil, fmt.Errorf("resolve import %q: %w", path, err)
		}
		delete(l.states, path)
	}

	if l.fallback != nil {
		var (
			pkg *types.Package
			err error
		)
		if from, ok := l.fallback.(types.ImporterFrom); ok {
			pkg, err = from.ImportFrom(path, srcDir, mode)
		} else {
			pkg, err = l.fallback.Import(path)
		}
		if err != nil {
			return nil, err
		}
		if pkg == nil {
			return nil, fmt.Errorf("native importer returned a nil package for %q", path)
		}
		l.native[path] = pkg
		return pkg, nil
	}

	return nil, fmt.Errorf("golark: import %q: %w (provide it with Options.Resolver or Options.Importer)", path, ErrPackageNotFound)
}

func (l *packageLoader) checkSource(path string, files map[string][]byte) (*checker.Checked, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("package %q has no Go source files", path)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name == "" {
			return nil, fmt.Errorf("package %q contains an empty filename", path)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	parsed := make([]*ast.File, 0, len(names))
	for _, name := range names {
		if err := l.ctx.Err(); err != nil {
			return nil, err
		}
		file, err := syntax.ParseFile(l.fset, name, files[name])
		if err != nil {
			return nil, fmt.Errorf("parse package %q (%s): %w", path, name, err)
		}
		parsed = append(parsed, file)
	}
	return checker.Check(&checker.Program{Fset: l.fset, Files: parsed}, checker.Options{
		PackagePath: path,
		Importer:    l,
	})
}

func (l *packageLoader) importCycle(path string) error {
	start := 0
	for i, item := range l.stack {
		if item == path {
			start = i
			break
		}
	}
	cycle := append(append([]string(nil), l.stack[start:]...), path)
	return fmt.Errorf("import cycle not allowed: %v", cycle)
}
