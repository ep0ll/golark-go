// Package eval contains Golark's AST evaluator. Parsing and type checking are
// delegated to the Go standard library; this package provides execution.
package eval

import (
	"context"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ep0ll/golark-go/internal/checker"
)

// Options configures the runtime. Imports maps an import path to the runtime
// values exposed by that imported package. The corresponding static package
// information is provided by checker.Options.Importer.
type Options struct {
	Predeclared map[string]checker.Binding
	Imports     map[string]map[string]checker.Binding
	MaxSteps    int64
}

type Runtime struct {
	checked *checker.Checked
	opts    Options
	globals *env
	steps   atomic.Int64
	max     int64
}

// Module is an initialized package instance. Its functions close over the
// module's global environment; callers may invoke them concurrently when the
// host functions and values they share are themselves concurrency-safe.
type Module struct {
	runtime *Runtime
}

type Function struct {
	name string
	decl *ast.FuncDecl
	lit  *ast.FuncLit
	env  *env
	rt   *Runtime
	sig  *types.Signature
}

type tuple []any

type flowKind uint8

const (
	flowNormal flowKind = iota
	flowReturn
	flowBreak
	flowContinue
)

type flow struct {
	kind   flowKind
	values []any
}

// New creates an evaluator for a checked program. It does not execute the
// package until Init is called.
func New(checked *checker.Checked, opts Options) (*Runtime, error) {
	if checked == nil || checked.Program == nil {
		return nil, fmt.Errorf("golark: nil checked program")
	}
	max := opts.MaxSteps
	if max == 0 {
		max = 5_000_000
	}
	rt := &Runtime{checked: checked, opts: opts, globals: newEnv(nil), max: max}
	for name, binding := range opts.Predeclared {
		rt.globals.define(name, binding.Value)
	}
	if err := rt.bindImports(); err != nil {
		return nil, err
	}
	return rt, nil
}

// Init initializes package variables and registers functions.
func (rt *Runtime) Init(ctx context.Context) (*Module, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Register all package functions before initialization so forward calls and
	// recursion work. init functions are special and may occur more than once.
	var initFunctions []*ast.FuncDecl
	for _, file := range rt.checked.Program.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if fn.Name.Name == "init" {
				initFunctions = append(initFunctions, fn)
				continue
			}
			obj := rt.checked.Info.Defs[fn.Name]
			var sig *types.Signature
			if obj != nil {
				sig, _ = obj.Type().Underlying().(*types.Signature)
			}
			rt.globals.define(fn.Name.Name, &Function{name: fn.Name.Name, decl: fn, env: rt.globals, rt: rt, sig: sig})
		}
	}
	// Constants are compile-time values and are available while evaluating vars.
	for _, file := range rt.checked.Program.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, name := range vs.Names {
					obj, ok := rt.checked.Info.Defs[name].(*types.Const)
					if !ok {
						continue
					}
					value, err := constantToValue(obj.Val(), obj.Type())
					if err == nil {
						value, err = convertToType(value, obj.Type())
					}
					if err != nil {
						return nil, fmt.Errorf("%s: %w", rt.checked.Program.Fset.Position(name.Pos()), err)
					}
					rt.globals.define(name.Name, value)
				}
			}
		}
	}
	// Declare package variables with their zero values before running any
	// initializers. go/types supplies dependency ordering for initialized vars.
	for _, file := range rt.checked.Program.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, name := range vs.Names {
					obj, ok := rt.checked.Info.Defs[name].(*types.Var)
					if !ok {
						continue
					}
					zero := any(nil)
					if typ := reflectTypeFor(obj.Type()); typ != nil {
						zero = reflect.Zero(typ).Interface()
					}
					rt.globals.define(name.Name, zero)
				}
			}
		}
	}
	for _, init := range rt.checked.Info.InitOrder {
		if err := rt.tick(ctx); err != nil {
			return nil, err
		}
		value, err := rt.evalExpr(ctx, init.Rhs, rt.globals)
		if err != nil {
			return nil, err
		}
		values, ok := value.(tuple)
		if !ok {
			values = tuple{value}
		}
		if len(values) != len(init.Lhs) {
			return nil, fmt.Errorf("package initializer produced %d values for %d variables", len(values), len(init.Lhs))
		}
		for i, lhs := range init.Lhs {
			value, err := convertToType(values[i], lhs.Type())
			if err != nil {
				return nil, fmt.Errorf("initialize %s: %w", lhs.Name(), err)
			}
			if err := rt.globals.assign(lhs.Name(), value); err != nil {
				return nil, err
			}
		}
	}
	for _, decl := range initFunctions {
		fn := &Function{name: "init", decl: decl, env: rt.globals, rt: rt}
		if _, err := rt.callFunction(ctx, fn, nil); err != nil {
			return nil, err
		}
	}
	return &Module{runtime: rt}, nil
}

func (rt *Runtime) bindImports() error {
	for _, file := range rt.checked.Program.Files {
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			values := rt.opts.Imports[path]
			alias := ""
			if spec.Name != nil {
				alias = spec.Name.Name
			} else if pkgName, ok := rt.checked.Info.Implicits[spec].(*types.PkgName); ok {
				// Import paths do not necessarily end with the package name.
				alias = pkgName.Imported().Name()
			} else {
				parts := strings.Split(path, "/")
				alias = parts[len(parts)-1]
			}
			if alias == "_" {
				continue
			}
			if alias == "." {
				for name, binding := range values {
					rt.globals.define(name, binding.Value)
				}
				continue
			}
			ns := make(map[string]any, len(values))
			for name, binding := range values {
				ns[name] = binding.Value
			}
			rt.globals.define(alias, ns)
		}
	}
	return nil
}

func (rt *Runtime) tick(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if rt.steps.Add(1) > rt.max {
		return fmt.Errorf("golark: execution step limit (%d) exceeded", rt.max)
	}
	return nil
}

func (m *Module) Globals() map[string]any {
	out := make(map[string]any, len(m.runtime.globals.values))
	for name, cell := range m.runtime.globals.values {
		if !strings.HasPrefix(name, "_") {
			out[name] = cell.value
		}
	}
	return out
}

func (m *Module) Lookup(name string) (any, bool) { return m.runtime.globals.get(name) }

// Call invokes a top-level script function by name. Multiple Go return values
// are returned as separate entries in the result slice.
func (m *Module) Call(ctx context.Context, name string, args ...any) ([]any, error) {
	value, ok := m.runtime.globals.get(name)
	if !ok {
		return nil, fmt.Errorf("golark: no global named %q", name)
	}
	result, err := m.runtime.callValue(ctx, value, args)
	if err != nil {
		return nil, err
	}
	if values, ok := result.(tuple); ok {
		return []any(values), nil
	}
	if result == nil {
		return nil, nil
	}
	return []any{result}, nil
}

// CallValue invokes a callable value retrieved from Module.Lookup.
func (m *Module) CallValue(ctx context.Context, callable any, args ...any) ([]any, error) {
	result, err := m.runtime.callValue(ctx, callable, args)
	if err != nil {
		return nil, err
	}
	if values, ok := result.(tuple); ok {
		return []any(values), nil
	}
	if result == nil {
		return nil, nil
	}
	return []any{result}, nil
}

func (rt *Runtime) callValue(ctx context.Context, callable any, args []any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := rt.tick(ctx); err != nil {
		return nil, err
	}
	switch fn := callable.(type) {
	case *Function:
		return rt.callFunction(ctx, fn, args)
	case reflect.Value:
		return callReflect(ctx, fn, args)
	default:
		rv := indirect(callable)
		if rv.IsValid() && rv.Kind() == reflect.Func {
			return callReflect(ctx, rv, args)
		}
		return nil, fmt.Errorf("golark: value of type %T is not callable", callable)
	}
}

func (rt *Runtime) callFunction(ctx context.Context, fn *Function, args []any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn.sig != nil && !fn.sig.Variadic() && len(args) != fn.sig.Params().Len() {
		return nil, fmt.Errorf("%s: got %d arguments, want %d", fn.name, len(args), fn.sig.Params().Len())
	}
	if fn.sig != nil && fn.sig.Variadic() && len(args) < fn.sig.Params().Len()-1 {
		return nil, fmt.Errorf("%s: not enough arguments", fn.name)
	}
	local := newEnv(fn.env)
	fields := []*ast.Field(nil)
	if fn.decl != nil {
		fields = fn.decl.Type.Params.List
	} else {
		fields = fn.lit.Type.Params.List
	}
	argIndex, paramIndex := 0, 0
	for _, field := range fields {
		if len(field.Names) == 0 {
			if fn.sig != nil && fn.sig.Variadic() && paramIndex == fn.sig.Params().Len()-1 {
				argIndex = len(args)
			} else {
				argIndex++
			}
			paramIndex++
			continue
		}
		for _, name := range field.Names {
			variadic := fn.sig != nil && fn.sig.Variadic() && paramIndex == fn.sig.Params().Len()-1
			if argIndex >= len(args) && !variadic {
				return nil, fmt.Errorf("%s: missing argument %q", fn.name, name.Name)
			}
			var value any
			if argIndex < len(args) {
				value = args[argIndex]
			}
			var err error
			if fn.sig != nil && paramIndex < fn.sig.Params().Len() {
				paramType := fn.sig.Params().At(paramIndex).Type()
				if variadic {
					sliceType := reflectTypeFor(paramType)
					if sliceType == nil || sliceType.Kind() != reflect.Slice {
						return nil, fmt.Errorf("%s: unsupported variadic parameter type", fn.name)
					}
					if argIndex == len(args) {
						value = reflect.MakeSlice(sliceType, 0, 0).Interface()
					} else if argIndex == len(args)-1 {
						if rv := indirect(value); rv.IsValid() && rv.Type().AssignableTo(sliceType) {
							converted, convErr := toReflect(value, sliceType)
							if convErr != nil {
								return nil, convErr
							}
							value = converted.Interface()
							argIndex++
						} else {
							value, argIndex, err = collectVariadic(args, argIndex, sliceType)
							if err != nil {
								return nil, err
							}
						}
					} else {
						value, argIndex, err = collectVariadic(args, argIndex, sliceType)
						if err != nil {
							return nil, err
						}
					}
				} else {
					converted, err := convertToType(value, paramType)
					if err != nil {
						return nil, fmt.Errorf("argument %s: %w", name.Name, err)
					}
					value = converted
					argIndex++
				}
			} else {
				argIndex++
			}
			local.define(name.Name, value)
			paramIndex++
		}
	}
	if argIndex < len(args) {
		return nil, fmt.Errorf("%s: too many arguments", fn.name)
	}
	var body *ast.BlockStmt
	if fn.decl != nil {
		body = fn.decl.Body
	} else {
		body = fn.lit.Body
	}
	f := &frame{rt: rt, env: local, ctx: ctx}
	out, err := f.execBlock(body)
	if err != nil {
		return nil, err
	}
	if out.kind == flowReturn {
		if fn.sig != nil {
			for i := range out.values {
				if i < fn.sig.Results().Len() {
					converted, err := convertToType(out.values[i], fn.sig.Results().At(i).Type())
					if err != nil {
						return nil, fmt.Errorf("return value %d: %w", i+1, err)
					}
					out.values[i] = converted
				}
			}
		}
		if len(out.values) == 0 {
			return nil, nil
		}
		if len(out.values) == 1 {
			return out.values[0], nil
		}
		return tuple(out.values), nil
	}
	return nil, nil
}

func collectVariadic(args []any, start int, sliceType reflect.Type) (any, int, error) {
	slice := reflect.MakeSlice(sliceType, 0, len(args)-start)
	for i := start; i < len(args); i++ {
		v, err := toReflect(args[i], sliceType.Elem())
		if err != nil {
			return nil, start, err
		}
		slice = reflect.Append(slice, v)
	}
	return slice.Interface(), len(args), nil
}

func callReflect(ctx context.Context, fn reflect.Value, args []any) (any, error) {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return nil, fmt.Errorf("golark: invalid Go function")
	}
	t := fn.Type()
	if !t.IsVariadic() && len(args) != t.NumIn() {
		return nil, fmt.Errorf("got %d arguments, want %d", len(args), t.NumIn())
	}
	if t.IsVariadic() && len(args) < t.NumIn()-1 {
		return nil, fmt.Errorf("not enough arguments")
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		target := t.In(i)
		if t.IsVariadic() && i >= t.NumIn()-1 {
			target = t.In(t.NumIn() - 1).Elem()
		}
		v, err := toReflectContext(arg, target, ctx)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		in[i] = v
	}
	var out []reflect.Value
	if t.IsVariadic() {
		out = fn.Call(in)
	} else {
		out = fn.Call(in)
	}
	values := make([]any, len(out))
	for i, v := range out {
		values[i] = v.Interface()
	}
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return tuple(values), nil
}

func convertToType(value any, typ types.Type) (any, error) {
	target := reflectTypeFor(typ)
	if target == nil {
		return value, nil
	}
	rv, err := toReflect(value, target)
	if err != nil {
		return nil, err
	}
	return rv.Interface(), nil
}

func constantToValue(v constant.Value, typ types.Type) (any, error) {
	if v == nil {
		return nil, nil
	}
	if b, ok := typ.Underlying().(*types.Basic); ok {
		switch b.Kind() {
		case types.Bool, types.UntypedBool:
			return constant.BoolVal(v), nil
		case types.String, types.UntypedString:
			return constant.StringVal(v), nil
		case types.Int, types.UntypedInt:
			n, ok := constant.Int64Val(constant.ToInt(v))
			if !ok {
				return nil, fmt.Errorf("integer constant is outside runtime range")
			}
			return int(n), nil
		case types.Int8:
			n, ok := constant.Int64Val(constant.ToInt(v))
			if !ok {
				return nil, fmt.Errorf("integer constant is outside runtime range")
			}
			return int8(n), nil
		case types.Int16:
			n, ok := constant.Int64Val(constant.ToInt(v))
			if !ok {
				return nil, fmt.Errorf("integer constant is outside runtime range")
			}
			return int16(n), nil
		case types.Int32, types.UntypedRune:
			n, ok := constant.Int64Val(constant.ToInt(v))
			if !ok {
				return nil, fmt.Errorf("integer constant is outside runtime range")
			}
			return int32(n), nil
		case types.Int64:
			n, ok := constant.Int64Val(constant.ToInt(v))
			if !ok {
				return nil, fmt.Errorf("integer constant is outside runtime range")
			}
			return n, nil
		case types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr:
			if n, ok := constant.Uint64Val(constant.ToInt(v)); ok {
				return n, nil
			}
			if n, ok := constant.Int64Val(constant.ToInt(v)); ok {
				return n, nil
			}
			f, _ := constant.Float64Val(v)
			return f, nil
		case types.UntypedFloat, types.Float64:
			f, ok := constant.Float64Val(constant.ToFloat(v))
			if !ok {
				return nil, fmt.Errorf("floating-point constant is outside runtime range")
			}
			if typ.Underlying().(*types.Basic).Kind() == types.Float32 {
				return float32(f), nil
			}
			return f, nil
		case types.Complex64, types.Complex128, types.UntypedComplex:
			re, okRe := constant.Float64Val(constant.Real(v))
			im, okIm := constant.Float64Val(constant.Imag(v))
			if !okRe || !okIm {
				return nil, fmt.Errorf("complex constant is outside runtime range")
			}
			return complex(re, im), nil
		}
	}
	return nil, fmt.Errorf("unsupported constant type %s", typ)
}

func basicLiteral(lit *ast.BasicLit, typ types.Type) (any, error) {
	v := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
	if v.Kind() == constant.Unknown {
		return nil, fmt.Errorf("invalid literal %q", lit.Value)
	}
	return constantToValue(v, typ)
}
