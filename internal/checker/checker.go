// Package checker performs Go's static analysis for Golark programs.
package checker

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"reflect"
	"sort"
)

// Binding associates a runtime value with the static type visible to scripts.
// Type may be supplied for values whose Go reflection type is not sufficient
// (for example, values involving type parameters or an application-specific
// named type). A nil Type is inferred from Value.
type Binding struct {
	Value any
	Type  types.Type
}

// Bind creates a binding and infers its Go type from the runtime value.
func Bind(value any) Binding {
	return Binding{Value: value}
}

// Options controls package checking.
type Options struct {
	PackagePath string
	Importer    types.Importer
	Predeclared map[string]Binding
}

// Program is the syntax-only representation of one Go package.
type Program struct {
	Fset  *token.FileSet
	Files []*ast.File
}

// Checked is a successfully type-checked program.
type Checked struct {
	Program *Program
	Info    *types.Info
	Package *types.Package
}

// Check type-checks every file as one Go package. Predeclared bindings are
// inserted into the package scope before checking, just like embedded host APIs.
func Check(program *Program, opts Options) (*Checked, error) {
	if program == nil || program.Fset == nil || len(program.Files) == 0 {
		return nil, fmt.Errorf("golark: cannot check an empty program")
	}
	packageName := program.Files[0].Name.Name
	for _, file := range program.Files[1:] {
		if file.Name.Name != packageName {
			return nil, fmt.Errorf("golark: files in one module must declare the same package")
		}
	}
	path := opts.PackagePath
	if path == "" {
		path = "golark.local/" + packageName
	}
	pkg := types.NewPackage(path, packageName)

	keys := make([]string, 0, len(opts.Predeclared))
	for name := range opts.Predeclared {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	seenTypes := make(map[reflect.Type]types.Type)
	seenPackages := make(map[string]*types.Package)
	typeImporter := opts.Importer
	if typeImporter == nil {
		typeImporter = importer.Default()
	}
	for _, name := range keys {
		if !token.IsIdentifier(name) || token.Lookup(name).IsKeyword() || name == "_" {
			return nil, fmt.Errorf("golark: invalid predeclared identifier %q", name)
		}
		binding := opts.Predeclared[name]
		typ := binding.Type
		if typ == nil {
			if binding.Value == nil {
				return nil, fmt.Errorf("golark: predeclared binding %q has nil value and no explicit type", name)
			}
			var err error
			typ, err = reflectType(reflect.TypeOf(binding.Value), seenTypes, seenPackages, typeImporter)
			if err != nil {
				return nil, fmt.Errorf("golark: infer type of predeclared %q: %w", name, err)
			}
		}
		var obj types.Object
		if sig, ok := typ.Underlying().(*types.Signature); ok {
			obj = types.NewFunc(token.NoPos, pkg, name, sig)
		} else {
			obj = types.NewVar(token.NoPos, pkg, name, typ)
		}
		if alt := pkg.Scope().Insert(obj); alt != nil {
			return nil, fmt.Errorf("golark: duplicate predeclared name %q", name)
		}
	}

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	imp := opts.Importer
	if imp == nil {
		imp = importer.Default()
	}
	conf := &types.Config{Importer: imp, GoVersion: "go1.23"}
	checker := types.NewChecker(conf, program.Fset, pkg, info)
	if err := checker.Files(program.Files); err != nil {
		return nil, err
	}
	pkg.MarkComplete()
	return &Checked{Program: program, Info: info, Package: pkg}, nil
}

func reflectType(rt reflect.Type, seen map[reflect.Type]types.Type, packages map[string]*types.Package, imp types.Importer) (types.Type, error) {
	if rt == nil {
		return nil, fmt.Errorf("nil reflect.Type")
	}
	if t, ok := seen[rt]; ok {
		return t, nil
	}
	if rt.Name() != "" && rt.PkgPath() == "" {
		if b := reflectBasic(rt.Kind()); b != nil {
			return b, nil
		}
	}
	if rt.Name() != "" && rt.PkgPath() != "" {
		if imported, err := imp.Import(rt.PkgPath()); err == nil {
			if obj := imported.Scope().Lookup(rt.Name()); obj != nil {
				seen[rt] = obj.Type()
				return obj.Type(), nil
			}
		}
		pkg := packages[rt.PkgPath()]
		if pkg == nil {
			pkg = types.NewPackage(rt.PkgPath(), packageName(rt.PkgPath()))
			packages[rt.PkgPath()] = pkg
		}
		if obj := pkg.Scope().Lookup(rt.Name()); obj != nil {
			if obj, ok := obj.(*types.TypeName); ok {
				seen[rt] = obj.Type()
				return obj.Type(), nil
			}
		}
		obj := types.NewTypeName(token.NoPos, pkg, rt.Name(), nil)
		if alt := pkg.Scope().Insert(obj); alt != nil {
			if tn, ok := alt.(*types.TypeName); ok {
				seen[rt] = tn.Type()
				return tn.Type(), nil
			}
		}
		named := types.NewNamed(obj, types.Typ[types.Invalid], nil)
		seen[rt] = named
		underlying, err := reflectUnderlying(rt, seen, packages, imp)
		if err != nil {
			return nil, err
		}
		named.SetUnderlying(underlying)
		for i := 0; i < rt.NumMethod(); i++ {
			m := rt.Method(i)
			params := make([]*types.Var, 0, m.Type.NumIn()-1)
			for p := 1; p < m.Type.NumIn(); p++ {
				mt, err := reflectType(m.Type.In(p), seen, packages, imp)
				if err != nil {
					return nil, err
				}
				params = append(params, types.NewVar(token.NoPos, pkg, "", mt))
			}
			results := make([]*types.Var, m.Type.NumOut())
			for r := range results {
				mt, err := reflectType(m.Type.Out(r), seen, packages, imp)
				if err != nil {
					return nil, err
				}
				results[r] = types.NewVar(token.NoPos, pkg, "", mt)
			}
			recvType, err := reflectType(m.Type.In(0), seen, packages, imp)
			if err != nil {
				return nil, err
			}
			recv := types.NewVar(token.NoPos, pkg, "", recvType)
			sig := types.NewSignatureType(recv, nil, nil, types.NewTuple(params...), types.NewTuple(results...), m.Type.IsVariadic())
			named.AddMethod(types.NewFunc(token.NoPos, pkg, m.Name, sig))
		}
		return named, nil
	}
	return reflectUnderlying(rt, seen, packages, imp)
}

func reflectUnderlying(rt reflect.Type, seen map[reflect.Type]types.Type, packages map[string]*types.Package, imp types.Importer) (types.Type, error) {
	if b := reflectBasic(rt.Kind()); b != nil {
		return b, nil
	}
	child := func(t reflect.Type) (types.Type, error) { return reflectType(t, seen, packages, imp) }
	switch rt.Kind() {
	case reflect.Pointer:
		e, err := child(rt.Elem())
		if err != nil {
			return nil, err
		}
		return types.NewPointer(e), nil
	case reflect.Slice:
		e, err := child(rt.Elem())
		if err != nil {
			return nil, err
		}
		return types.NewSlice(e), nil
	case reflect.Array:
		e, err := child(rt.Elem())
		if err != nil {
			return nil, err
		}
		return types.NewArray(e, int64(rt.Len())), nil
	case reflect.Map:
		k, err := child(rt.Key())
		if err != nil {
			return nil, err
		}
		e, err := child(rt.Elem())
		if err != nil {
			return nil, err
		}
		return types.NewMap(k, e), nil
	case reflect.Chan:
		e, err := child(rt.Elem())
		if err != nil {
			return nil, err
		}
		dir := types.SendRecv
		if rt.ChanDir() == reflect.RecvDir {
			dir = types.RecvOnly
		} else if rt.ChanDir() == reflect.SendDir {
			dir = types.SendOnly
		}
		return types.NewChan(dir, e), nil
	case reflect.Func:
		params := make([]*types.Var, rt.NumIn())
		for i := range params {
			t, err := child(rt.In(i))
			if err != nil {
				return nil, err
			}
			params[i] = types.NewVar(token.NoPos, nil, "", t)
		}
		results := make([]*types.Var, rt.NumOut())
		for i := range results {
			t, err := child(rt.Out(i))
			if err != nil {
				return nil, err
			}
			results[i] = types.NewVar(token.NoPos, nil, "", t)
		}
		return types.NewSignatureType(nil, nil, nil, types.NewTuple(params...), types.NewTuple(results...), rt.IsVariadic()), nil
	case reflect.Struct:
		fields := make([]*types.Var, rt.NumField())
		tags := make([]string, rt.NumField())
		for i := range fields {
			f := rt.Field(i)
			t, err := child(f.Type)
			if err != nil {
				return nil, err
			}
			fields[i] = types.NewField(token.NoPos, nil, f.Name, t, f.Anonymous)
			tags[i] = string(f.Tag)
		}
		return types.NewStruct(fields, tags), nil
	case reflect.Interface:
		methods := make([]*types.Func, rt.NumMethod())
		for i := range methods {
			m := rt.Method(i)
			t, err := child(m.Type)
			if err != nil {
				return nil, err
			}
			sig, ok := t.(*types.Signature)
			if !ok {
				return nil, fmt.Errorf("invalid method signature %s", m.Name)
			}
			methods[i] = types.NewFunc(token.NoPos, nil, m.Name, sig)
		}
		return types.NewInterfaceType(methods, nil).Complete(), nil
	case reflect.UnsafePointer:
		return types.Typ[types.UnsafePointer], nil
	default:
		return nil, fmt.Errorf("unsupported Go value type %s", rt)
	}
}

func reflectBasic(k reflect.Kind) *types.Basic {
	var kind types.BasicKind
	switch k {
	case reflect.Bool:
		kind = types.Bool
	case reflect.Int:
		kind = types.Int
	case reflect.Int8:
		kind = types.Int8
	case reflect.Int16:
		kind = types.Int16
	case reflect.Int32:
		kind = types.Int32
	case reflect.Int64:
		kind = types.Int64
	case reflect.Uint:
		kind = types.Uint
	case reflect.Uint8:
		kind = types.Uint8
	case reflect.Uint16:
		kind = types.Uint16
	case reflect.Uint32:
		kind = types.Uint32
	case reflect.Uint64:
		kind = types.Uint64
	case reflect.Uintptr:
		kind = types.Uintptr
	case reflect.Float32:
		kind = types.Float32
	case reflect.Float64:
		kind = types.Float64
	case reflect.Complex64:
		kind = types.Complex64
	case reflect.Complex128:
		kind = types.Complex128
	case reflect.String:
		kind = types.String
	default:
		return nil
	}
	return types.Typ[kind]
}

func packageName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
