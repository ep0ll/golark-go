package eval

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"unsafe"
)

func (rt *Runtime) evalExpr(ctx context.Context, expr ast.Expr, scope *env) (any, error) {
	if err := rt.tick(ctx); err != nil {
		return nil, err
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		return basicLiteral(e, rt.checked.Info.TypeOf(e))
	case *ast.Ident:
		if value, ok := scope.get(e.Name); ok {
			return value, nil
		}
		if e.Name == "nil" {
			return nil, nil
		}
		if e.Name == "true" {
			return true, nil
		}
		if e.Name == "false" {
			return false, nil
		}
		if obj := rt.checked.Info.Uses[e]; obj != nil {
			if c, ok := obj.(*types.Const); ok {
				return constantToValue(c.Val(), c.Type())
			}
		}
		return nil, fmt.Errorf("%s: undefined runtime name %q", rt.checked.Program.Fset.Position(e.Pos()), e.Name)
	case *ast.ParenExpr:
		return rt.evalExpr(ctx, e.X, scope)
	case *ast.BinaryExpr:
		left, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		if e.Op == token.LAND && !truthy(left) {
			return false, nil
		}
		if e.Op == token.LOR && truthy(left) {
			return true, nil
		}
		right, err := rt.evalExpr(ctx, e.Y, scope)
		if err != nil {
			return nil, err
		}
		return binaryValue(e.Op, left, right)
	case *ast.UnaryExpr:
		value, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case token.NOT:
			return !truthy(value), nil
		case token.ADD:
			return value, nil
		case token.SUB:
			return unaryNumeric(value, token.SUB)
		case token.XOR:
			return unaryNumeric(value, token.XOR)
		case token.AND:
			rv := indirect(value)
			if !rv.IsValid() {
				return nil, fmt.Errorf("cannot take address of nil")
			}
			p := reflect.New(rv.Type())
			p.Elem().Set(rv)
			return p.Interface(), nil
		case token.MUL:
			rv := indirect(value)
			if rv.Kind() != reflect.Pointer || rv.IsNil() {
				return nil, fmt.Errorf("cannot dereference %T", value)
			}
			return rv.Elem().Interface(), nil
		default:
			return nil, fmt.Errorf("unary operator %s is not supported", e.Op)
		}
	case *ast.SelectorExpr:
		base, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		return selectorValue(base, e.Sel.Name)
	case *ast.IndexExpr:
		base, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		index, err := rt.evalExpr(ctx, e.Index, scope)
		if err != nil {
			return nil, err
		}
		return indexValue(base, index)
	case *ast.IndexListExpr:
		return nil, fmt.Errorf("generic instantiation is type-checked but not yet interpreted")
	case *ast.SliceExpr:
		base, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		return sliceValue(rt, ctx, base, e, scope)
	case *ast.CallExpr:
		return rt.evalCall(ctx, e, scope)
	case *ast.CompositeLit:
		return rt.evalComposite(ctx, e, scope)
	case *ast.FuncLit:
		sig, _ := rt.checked.Info.TypeOf(e.Type).Underlying().(*types.Signature)
		return &Function{name: "<func literal>", lit: e, env: scope, rt: rt, sig: sig}, nil
	case *ast.TypeAssertExpr:
		v, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		if e.Type == nil {
			return nil, fmt.Errorf("type switch assertion is not supported here")
		}
		want := rt.checked.Info.TypeOf(e.Type)
		rv, err := toReflect(v, reflectTypeFor(want))
		if err != nil {
			return nil, fmt.Errorf("type assertion failed: %w", err)
		}
		return rv.Interface(), nil
	case *ast.StarExpr:
		v, err := rt.evalExpr(ctx, e.X, scope)
		if err != nil {
			return nil, err
		}
		rv := indirect(v)
		if rv.Kind() != reflect.Pointer || rv.IsNil() {
			return nil, fmt.Errorf("cannot dereference %T", v)
		}
		return rv.Elem().Interface(), nil
	case *ast.KeyValueExpr:
		k, err := rt.evalExpr(ctx, e.Key, scope)
		if err != nil {
			return nil, err
		}
		v, err := rt.evalExpr(ctx, e.Value, scope)
		if err != nil {
			return nil, err
		}
		return [2]any{k, v}, nil
	case *ast.Ellipsis:
		return nil, fmt.Errorf("ellipsis is only supported in call arguments")
	default:
		return nil, fmt.Errorf("%s: expression %T is not yet supported by the evaluator", rt.checked.Program.Fset.Position(expr.Pos()), expr)
	}
}

func (rt *Runtime) evalCall(ctx context.Context, call *ast.CallExpr, scope *env) (any, error) {
	if id, ok := call.Fun.(*ast.Ident); ok {
		if id.Name == "make" {
			return rt.evalMake(ctx, call, scope)
		}
		if id.Name == "new" {
			return rt.evalNew(ctx, call)
		}
		if isBuiltin(id.Name) {
			args, err := rt.evalCallArgs(ctx, call.Args, scope)
			if err != nil {
				return nil, err
			}
			return callBuiltin(id.Name, args)
		}
		if obj := rt.checked.Info.Uses[id]; obj != nil {
			if typ, ok := obj.(*types.TypeName); ok {
				v, err := rt.evalCallArgs(ctx, call.Args, scope)
				if err != nil {
					return nil, err
				}
				if len(v) != 1 {
					return nil, fmt.Errorf("conversion to %s expects one argument", typ.Name())
				}
				rv, err := toReflect(v[0], reflectTypeFor(typ.Type()))
				if err != nil {
					return nil, err
				}
				return rv.Interface(), nil
			}
		}
	}
	// A call whose callee is a type expression may not have an Ident callee,
	// for example []byte(s) or *T(p).
	if tv, ok := rt.checked.Info.Types[call.Fun]; ok && tv.IsType() {
		args, err := rt.evalCallArgs(ctx, call.Args, scope)
		if err != nil {
			return nil, err
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("type conversion expects one argument")
		}
		rv, err := toReflect(args[0], reflectTypeFor(tv.Type))
		if err != nil {
			return nil, err
		}
		return rv.Interface(), nil
	}
	callee, err := rt.evalExpr(ctx, call.Fun, scope)
	if err != nil {
		return nil, err
	}
	args, err := rt.evalCallArgs(ctx, call.Args, scope)
	if err != nil {
		return nil, err
	}
	if call.Ellipsis.IsValid() && len(args) > 0 {
		last := indirect(args[len(args)-1])
		if last.IsValid() && (last.Kind() == reflect.Slice || last.Kind() == reflect.Array) {
			expanded := make([]any, 0, len(args)-1+last.Len())
			expanded = append(expanded, args[:len(args)-1]...)
			for i := 0; i < last.Len(); i++ {
				expanded = append(expanded, last.Index(i).Interface())
			}
			args = expanded
		}
	}
	return rt.callValue(ctx, callee, args)
}

func (rt *Runtime) evalCallArgs(ctx context.Context, exprs []ast.Expr, scope *env) ([]any, error) {
	args := make([]any, 0, len(exprs))
	for _, expr := range exprs {
		v, err := rt.evalExpr(ctx, expr, scope)
		if err != nil {
			return nil, err
		}
		if many, ok := v.(tuple); ok {
			args = append(args, many...)
		} else {
			args = append(args, v)
		}
	}
	return args, nil
}

func (rt *Runtime) evalComposite(ctx context.Context, lit *ast.CompositeLit, scope *env) (any, error) {
	typ := rt.checked.Info.TypeOf(lit)
	rtType := reflectTypeFor(typ)
	if rtType == nil {
		return nil, fmt.Errorf("cannot construct runtime value of type %s", typ)
	}
	under := typ.Underlying()
	switch under.(type) {
	case *types.Slice:
		values := make([]any, 0, len(lit.Elts))
		for _, elt := range lit.Elts {
			v, err := rt.evalExpr(ctx, elt, scope)
			if err != nil {
				return nil, err
			}
			if kv, ok := v.([2]any); ok {
				v = kv[1]
			}
			values = append(values, v)
		}
		out := reflect.MakeSlice(rtType, len(values), len(values))
		for i, v := range values {
			x, err := toReflect(v, rtType.Elem())
			if err != nil {
				return nil, err
			}
			out.Index(i).Set(x)
		}
		return out.Interface(), nil
	case *types.Array:
		out := reflect.New(rtType).Elem()
		for i, elt := range lit.Elts {
			idx := i
			valueExpr := elt
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				iv, err := rt.evalExpr(ctx, kv.Key, scope)
				if err != nil {
					return nil, err
				}
				idx = int(toInt64(iv))
				valueExpr = kv.Value
			}
			v, err := rt.evalExpr(ctx, valueExpr, scope)
			if err != nil {
				return nil, err
			}
			x, err := toReflect(v, rtType.Elem())
			if err != nil {
				return nil, err
			}
			out.Index(idx).Set(x)
		}
		return out.Interface(), nil
	case *types.Map:
		out := reflect.MakeMapWithSize(rtType, len(lit.Elts))
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("map literal element must be key:value")
			}
			k, err := rt.evalExpr(ctx, kv.Key, scope)
			if err != nil {
				return nil, err
			}
			v, err := rt.evalExpr(ctx, kv.Value, scope)
			if err != nil {
				return nil, err
			}
			rk, err := toReflect(k, rtType.Key())
			if err != nil {
				return nil, err
			}
			rv, err := toReflect(v, rtType.Elem())
			if err != nil {
				return nil, err
			}
			out.SetMapIndex(rk, rv)
		}
		return out.Interface(), nil
	case *types.Struct:
		out := reflect.New(rtType).Elem()
		for i, elt := range lit.Elts {
			var field reflect.Value
			var valueExpr ast.Expr
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					return nil, fmt.Errorf("invalid struct literal key")
				}
				field = out.FieldByName(key.Name)
				valueExpr = kv.Value
			} else {
				if i >= out.NumField() {
					return nil, fmt.Errorf("too many struct literal elements")
				}
				field = out.Field(i)
				valueExpr = elt
			}
			if !field.IsValid() || !field.CanSet() {
				return nil, fmt.Errorf("struct field is not settable")
			}
			v, err := rt.evalExpr(ctx, valueExpr, scope)
			if err != nil {
				return nil, err
			}
			rv, err := toReflect(v, field.Type())
			if err != nil {
				return nil, err
			}
			field.Set(rv)
		}
		return out.Interface(), nil
	default:
		return nil, fmt.Errorf("composite literal for %s is not supported", typ)
	}
}

func (rt *Runtime) evalMake(ctx context.Context, call *ast.CallExpr, scope *env) (any, error) {
	if len(call.Args) == 0 {
		return nil, fmt.Errorf("make expects a type")
	}
	typ := rt.checked.Info.TypeOf(call.Args[0])
	rtType := reflectTypeFor(typ)
	if rtType == nil {
		return nil, fmt.Errorf("cannot make type %s", typ)
	}
	args, err := rt.evalCallArgs(ctx, call.Args[1:], scope)
	if err != nil {
		return nil, err
	}
	switch rtType.Kind() {
	case reflect.Slice:
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("make slice expects len and optional cap")
		}
		n := int(toInt64(args[0]))
		c := n
		if len(args) == 2 {
			c = int(toInt64(args[1]))
		}
		return reflect.MakeSlice(rtType, n, c).Interface(), nil
	case reflect.Map:
		capacity := 0
		if len(args) > 0 {
			capacity = int(toInt64(args[0]))
		}
		return reflect.MakeMapWithSize(rtType, capacity).Interface(), nil
	case reflect.Chan:
		capacity := 0
		if len(args) > 0 {
			capacity = int(toInt64(args[0]))
		}
		return reflect.MakeChan(rtType, capacity).Interface(), nil
	default:
		return nil, fmt.Errorf("make does not support %s", rtType)
	}
}

func (rt *Runtime) evalNew(ctx context.Context, call *ast.CallExpr) (any, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("new expects one type")
	}
	t := reflectTypeFor(rt.checked.Info.TypeOf(call.Args[0]))
	if t == nil {
		return nil, fmt.Errorf("cannot allocate requested type")
	}
	return reflect.New(t).Interface(), nil
}

func sliceValue(rt *Runtime, ctx context.Context, base any, e *ast.SliceExpr, scope *env) (any, error) {
	rv := indirect(base)
	if !rv.IsValid() {
		return nil, fmt.Errorf("cannot slice nil")
	}
	low, high, max := 0, -1, -1
	if e.Low != nil {
		v, err := rt.evalExpr(ctx, e.Low, scope)
		if err != nil {
			return nil, err
		}
		low = int(toInt64(v))
	}
	if e.High != nil {
		v, err := rt.evalExpr(ctx, e.High, scope)
		if err != nil {
			return nil, err
		}
		high = int(toInt64(v))
	}
	if e.Max != nil {
		v, err := rt.evalExpr(ctx, e.Max, scope)
		if err != nil {
			return nil, err
		}
		max = int(toInt64(v))
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Slice, reflect.String:
		if high < 0 {
			high = rv.Len()
		}
		if max >= 0 {
			return rv.Slice3(low, high, max).Interface(), nil
		}
		if rv.Kind() == reflect.String {
			return rv.String()[low:high], nil
		}
		return rv.Slice(low, high).Interface(), nil
	default:
		return nil, fmt.Errorf("cannot slice %T", base)
	}
}

func selectorValue(base any, name string) (any, error) {
	if ns, ok := base.(map[string]any); ok {
		v, found := ns[name]
		if !found {
			return nil, fmt.Errorf("package has no member %q", name)
		}
		return v, nil
	}
	rv := indirect(base)
	if !rv.IsValid() {
		return nil, fmt.Errorf("cannot select %s on nil", name)
	}
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil, fmt.Errorf("nil pointer selecting %s", name)
	}
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		field := rv.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			return field.Interface(), nil
		}
	}
	method := indirect(base).MethodByName(name)
	if method.IsValid() {
		return method, nil
	}
	return nil, fmt.Errorf("%T has no accessible field or method %q", base, name)
}

func indexValue(base, index any) (any, error) {
	rv := indirect(base)
	if !rv.IsValid() {
		return nil, fmt.Errorf("cannot index nil")
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Slice:
		i := int(toInt64(index))
		if i < 0 || i >= rv.Len() {
			return nil, fmt.Errorf("index %d out of range", i)
		}
		return rv.Index(i).Interface(), nil
	case reflect.String:
		i := int(toInt64(index))
		if i < 0 || i >= rv.Len() {
			return nil, fmt.Errorf("index %d out of range", i)
		}
		return rv.String()[i], nil
	case reflect.Map:
		k, err := toReflect(index, rv.Type().Key())
		if err != nil {
			return nil, err
		}
		v := rv.MapIndex(k)
		if !v.IsValid() {
			return reflect.Zero(rv.Type().Elem()).Interface(), nil
		}
		return v.Interface(), nil
	default:
		return nil, fmt.Errorf("cannot index %T", base)
	}
}

func assignIndex(base, index, value any) error {
	rv := indirect(base)
	if !rv.IsValid() {
		return fmt.Errorf("cannot assign through nil")
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		i := int(toInt64(index))
		if i < 0 || i >= rv.Len() {
			return fmt.Errorf("index %d out of range", i)
		}
		if !rv.Index(i).CanSet() {
			return fmt.Errorf("element is not settable")
		}
		v, err := toReflect(value, rv.Type().Elem())
		if err != nil {
			return err
		}
		rv.Index(i).Set(v)
		return nil
	case reflect.Map:
		k, err := toReflect(index, rv.Type().Key())
		if err != nil {
			return err
		}
		v, err := toReflect(value, rv.Type().Elem())
		if err != nil {
			return err
		}
		rv.SetMapIndex(k, v)
		return nil
	default:
		return fmt.Errorf("cannot assign index of %T", base)
	}
}

func isBuiltin(name string) bool {
	switch name {
	case "len", "cap", "append", "copy", "delete", "clear", "close", "complex", "real", "imag", "max", "min", "panic", "recover", "print", "println":
		return true
	}
	return false
}

func callBuiltin(name string, args []any) (any, error) {
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s expects %d arguments, got %d", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "len", "cap":
		if err := need(1); err != nil {
			return nil, err
		}
		rv := indirect(args[0])
		if !rv.IsValid() {
			return 0, nil
		}
		switch rv.Kind() {
		case reflect.Array, reflect.Slice, reflect.Map, reflect.Chan:
			if name == "cap" {
				return int64(rv.Cap()), nil
			}
			return int64(rv.Len()), nil
		case reflect.String:
			return int64(len(rv.String())), nil
		}
		return nil, fmt.Errorf("%s does not accept %T", name, args[0])
	case "append":
		if len(args) < 2 {
			return nil, fmt.Errorf("append expects at least two arguments")
		}
		rv := indirect(args[0])
		if !rv.IsValid() || rv.Kind() != reflect.Slice {
			return nil, fmt.Errorf("append expects a slice")
		}
		out := rv
		for _, arg := range args[1:] {
			v, err := toReflect(arg, rv.Type().Elem())
			if err != nil {
				return nil, err
			}
			out = reflect.Append(out, v)
		}
		return out.Interface(), nil
	case "copy":
		if err := need(2); err != nil {
			return nil, err
		}
		dst, src := indirect(args[0]), indirect(args[1])
		return int64(reflect.Copy(dst, src)), nil
	case "delete":
		if err := need(2); err != nil {
			return nil, err
		}
		m := indirect(args[0])
		if m.Kind() != reflect.Map {
			return nil, fmt.Errorf("delete expects a map")
		}
		k, err := toReflect(args[1], m.Type().Key())
		if err != nil {
			return nil, err
		}
		m.SetMapIndex(k, reflect.Value{})
		return nil, nil
	case "clear":
		if err := need(1); err != nil {
			return nil, err
		}
		rv := indirect(args[0])
		switch rv.Kind() {
		case reflect.Map:
			iter := rv.MapRange()
			for iter.Next() {
				rv.SetMapIndex(iter.Key(), reflect.Value{})
			}
		case reflect.Slice:
			for i := 0; i < rv.Len(); i++ {
				rv.Index(i).Set(reflect.Zero(rv.Type().Elem()))
			}
		}
		return nil, nil
	case "complex":
		if err := need(2); err != nil {
			return nil, err
		}
		return complex(toFloat64(args[0]), toFloat64(args[1])), nil
	case "real":
		if err := need(1); err != nil {
			return nil, err
		}
		switch v := args[0].(type) {
		case complex64:
			return real(v), nil
		case complex128:
			return real(v), nil
		}
		return args[0], nil
	case "imag":
		if err := need(1); err != nil {
			return nil, err
		}
		switch v := args[0].(type) {
		case complex64:
			return imag(v), nil
		case complex128:
			return imag(v), nil
		}
		return 0.0, nil
	case "max", "min":
		if len(args) == 0 {
			return nil, fmt.Errorf("%s expects at least one argument", name)
		}
		out := args[0]
		for _, v := range args[1:] {
			c, err := compare(out, v)
			if err != nil {
				return nil, err
			}
			if (name == "max" && c < 0) || (name == "min" && c > 0) {
				out = v
			}
		}
		return out, nil
	case "panic":
		if err := need(1); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("panic: %v", args[0])
	case "print", "println":
		if name == "println" {
			fmtPrintln(args...)
			return nil, nil
		}
		fmtPrint(args...)
		return nil, nil
	case "close":
		if err := need(1); err != nil {
			return nil, err
		}
		rv := indirect(args[0])
		if rv.Kind() != reflect.Chan {
			return nil, fmt.Errorf("close expects a channel")
		}
		rv.Close()
		return nil, nil
	case "recover":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown builtin %q", name)
}

func fmtPrint(args ...any)   { fmt.Print(args...) }
func fmtPrintln(args ...any) { fmt.Println(args...) }

func reflectTypeFor(t types.Type) reflect.Type {
	if t == nil {
		return nil
	}
	if n, ok := t.(*types.Named); ok {
		t = n.Underlying()
	}
	switch x := t.(type) {
	case *types.Basic:
		switch x.Kind() {
		case types.Bool:
			return reflect.TypeOf(false)
		case types.Int:
			return reflect.TypeOf(int(0))
		case types.Int8:
			return reflect.TypeOf(int8(0))
		case types.Int16:
			return reflect.TypeOf(int16(0))
		case types.Int32, types.UntypedRune:
			return reflect.TypeOf(int32(0))
		case types.Int64:
			return reflect.TypeOf(int64(0))
		case types.Uint:
			return reflect.TypeOf(uint(0))
		case types.Uint8:
			return reflect.TypeOf(uint8(0))
		case types.Uint16:
			return reflect.TypeOf(uint16(0))
		case types.Uint32:
			return reflect.TypeOf(uint32(0))
		case types.Uint64:
			return reflect.TypeOf(uint64(0))
		case types.Uintptr:
			return reflect.TypeOf(uintptr(0))
		case types.Float32:
			return reflect.TypeOf(float32(0))
		case types.Float64, types.UntypedFloat:
			return reflect.TypeOf(float64(0))
		case types.Complex64:
			return reflect.TypeOf(complex64(0))
		case types.Complex128, types.UntypedComplex:
			return reflect.TypeOf(complex128(0))
		case types.String, types.UntypedString:
			return reflect.TypeOf("")
		case types.UntypedBool:
			return reflect.TypeOf(false)
		case types.UntypedInt:
			return reflect.TypeOf(int(0))
		case types.UnsafePointer:
			return reflect.TypeOf(unsafePointerZero())
		}
	case *types.Slice:
		e := reflectTypeFor(x.Elem())
		if e != nil {
			return reflect.SliceOf(e)
		}
	case *types.Array:
		e := reflectTypeFor(x.Elem())
		if e != nil {
			return reflect.ArrayOf(int(x.Len()), e)
		}
	case *types.Map:
		k, v := reflectTypeFor(x.Key()), reflectTypeFor(x.Elem())
		if k != nil && v != nil {
			return reflect.MapOf(k, v)
		}
	case *types.Pointer:
		e := reflectTypeFor(x.Elem())
		if e != nil {
			return reflect.PointerTo(e)
		}
	case *types.Chan:
		e := reflectTypeFor(x.Elem())
		if e != nil {
			dir := reflect.BothDir
			if x.Dir() == types.RecvOnly {
				dir = reflect.RecvDir
			} else if x.Dir() == types.SendOnly {
				dir = reflect.SendDir
			}
			return reflect.ChanOf(dir, e)
		}
	case *types.Signature:
		in := make([]reflect.Type, x.Params().Len())
		out := make([]reflect.Type, x.Results().Len())
		for i := range in {
			in[i] = reflectTypeFor(x.Params().At(i).Type())
			if in[i] == nil {
				return nil
			}
		}
		for i := range out {
			out[i] = reflectTypeFor(x.Results().At(i).Type())
			if out[i] == nil {
				return nil
			}
		}
		return reflect.FuncOf(in, out, x.Variadic())
	case *types.Struct:
		fields := make([]reflect.StructField, x.NumFields())
		for i := range fields {
			f := x.Field(i)
			ft := reflectTypeFor(f.Type())
			if ft == nil || !f.Exported() {
				return nil
			}
			fields[i] = reflect.StructField{Name: f.Name(), Type: ft, Anonymous: f.Embedded(), Tag: reflect.StructTag(x.Tag(i))}
		}
		return reflect.StructOf(fields)
	case *types.Interface:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
	return nil
}

func unsafePointerZero() unsafe.Pointer { return nil }

func truthy(v any) bool {
	if v == nil {
		return false
	}
	rv := indirect(v)
	if !rv.IsValid() {
		return false
	}
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	case reflect.Complex64, reflect.Complex128:
		return rv.Complex() != 0
	case reflect.String:
		return rv.Len() != 0
	case reflect.Array:
		return rv.Len() != 0
	case reflect.Slice, reflect.Map, reflect.Chan:
		return !rv.IsNil() && rv.Len() != 0
	case reflect.Pointer, reflect.Interface, reflect.Func:
		return !rv.IsNil()
	}
	return true
}

func binaryValue(op token.Token, a, b any) (any, error) {
	if op == token.EQL {
		return equal(a, b), nil
	}
	if op == token.NEQ {
		return !equal(a, b), nil
	}
	if op == token.LAND {
		return truthy(a) && truthy(b), nil
	}
	if op == token.LOR {
		return truthy(a) || truthy(b), nil
	}
	if op == token.LSS || op == token.GTR || op == token.LEQ || op == token.GEQ {
		c, err := compare(a, b)
		if err != nil {
			return nil, err
		}
		switch op {
		case token.LSS:
			return c < 0, nil
		case token.GTR:
			return c > 0, nil
		case token.LEQ:
			return c <= 0, nil
		case token.GEQ:
			return c >= 0, nil
		}
	}
	if op == token.ADD {
		if x, ok := a.(string); ok {
			y, ok := b.(string)
			if !ok {
				return nil, fmt.Errorf("cannot add string and %T", b)
			}
			return x + y, nil
		}
	}
	av, bv := indirect(a), indirect(b)
	if !av.IsValid() || !bv.IsValid() {
		return nil, fmt.Errorf("operator %s does not accept nil", op)
	}
	if av.Kind() == reflect.Complex64 || av.Kind() == reflect.Complex128 {
		z := av.Complex()
		w := bv.Convert(av.Type()).Complex()
		switch op {
		case token.ADD:
			z += w
		case token.SUB:
			z -= w
		case token.MUL:
			z *= w
		case token.QUO:
			z /= w
		default:
			return nil, fmt.Errorf("operator %s is not valid for complex values", op)
		}
		out := reflect.New(av.Type()).Elem()
		out.SetComplex(z)
		return out.Interface(), nil
	}
	if av.Kind() == reflect.Float32 || av.Kind() == reflect.Float64 {
		x := av.Float()
		w := bv.Convert(av.Type()).Float()
		switch op {
		case token.ADD:
			x += w
		case token.SUB:
			x -= w
		case token.MUL:
			x *= w
		case token.QUO:
			x /= w
		default:
			return nil, fmt.Errorf("operator %s is not valid for floating point values", op)
		}
		out := reflect.New(av.Type()).Elem()
		out.SetFloat(x)
		return out.Interface(), nil
	}
	if isSigned(av.Kind()) {
		x := av.Int()
		w := bv.Convert(av.Type()).Int()
		switch op {
		case token.ADD:
			x += w
		case token.SUB:
			x -= w
		case token.MUL:
			x *= w
		case token.QUO:
			if w == 0 {
				return nil, fmt.Errorf("integer divide by zero")
			}
			x /= w
		case token.REM:
			if w == 0 {
				return nil, fmt.Errorf("integer divide by zero")
			}
			x %= w
		case token.AND:
			x &= w
		case token.OR:
			x |= w
		case token.XOR:
			x ^= w
		case token.AND_NOT:
			x &^= w
		case token.SHL:
			x <<= uint(w)
		case token.SHR:
			x >>= uint(w)
		default:
			return nil, fmt.Errorf("unsupported integer operator %s", op)
		}
		out := reflect.New(av.Type()).Elem()
		out.SetInt(x)
		return out.Interface(), nil
	}
	if isUnsigned(av.Kind()) {
		x := av.Uint()
		w := bv.Convert(av.Type()).Uint()
		switch op {
		case token.ADD:
			x += w
		case token.SUB:
			x -= w
		case token.MUL:
			x *= w
		case token.QUO:
			if w == 0 {
				return nil, fmt.Errorf("integer divide by zero")
			}
			x /= w
		case token.REM:
			if w == 0 {
				return nil, fmt.Errorf("integer divide by zero")
			}
			x %= w
		case token.AND:
			x &= w
		case token.OR:
			x |= w
		case token.XOR:
			x ^= w
		case token.AND_NOT:
			x &^= w
		case token.SHL:
			x <<= uint(w)
		case token.SHR:
			x >>= uint(w)
		default:
			return nil, fmt.Errorf("unsupported unsigned operator %s", op)
		}
		out := reflect.New(av.Type()).Elem()
		out.SetUint(x)
		return out.Interface(), nil
	}
	return nil, fmt.Errorf("operator %s is not supported for %T and %T", op, a, b)
}

func unaryNumeric(v any, op token.Token) (any, error) {
	rv := indirect(v)
	if !rv.IsValid() {
		return nil, fmt.Errorf("unary operator %s does not accept nil", op)
	}
	out := reflect.New(rv.Type()).Elem()
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if op == token.SUB {
			out.SetInt(-rv.Int())
		} else {
			out.SetInt(^rv.Int())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if op == token.SUB {
			return nil, fmt.Errorf("cannot negate unsigned integer")
		}
		out.SetUint(^rv.Uint())
	case reflect.Float32, reflect.Float64:
		if op != token.SUB {
			return nil, fmt.Errorf("invalid floating unary operator")
		}
		out.SetFloat(-rv.Float())
	case reflect.Complex64, reflect.Complex128:
		if op != token.SUB {
			return nil, fmt.Errorf("invalid complex unary operator")
		}
		out.SetComplex(-rv.Complex())
	default:
		return nil, fmt.Errorf("unary operator %s is not supported for %T", op, v)
	}
	return out.Interface(), nil
}

func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := indirect(a), indirect(b)
	if !av.IsValid() || !bv.IsValid() {
		return !av.IsValid() && !bv.IsValid()
	}
	if av.Type().ConvertibleTo(bv.Type()) {
		return reflect.DeepEqual(av.Convert(bv.Type()).Interface(), bv.Interface())
	}
	if bv.Type().ConvertibleTo(av.Type()) {
		return reflect.DeepEqual(av.Interface(), bv.Convert(av.Type()).Interface())
	}
	return reflect.DeepEqual(a, b)
}

func compare(a, b any) (int, error) {
	av, bv := indirect(a), indirect(b)
	if !av.IsValid() || !bv.IsValid() {
		return 0, fmt.Errorf("cannot compare nil")
	}
	if av.Kind() == reflect.String {
		if bv.Kind() != reflect.String {
			return 0, fmt.Errorf("incompatible comparison")
		}
		return strings.Compare(av.String(), bv.String()), nil
	}
	if isSigned(av.Kind()) {
		x, y := av.Int(), bv.Convert(av.Type()).Int()
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
		return 0, nil
	}
	if isUnsigned(av.Kind()) {
		x, y := av.Uint(), bv.Convert(av.Type()).Uint()
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
		return 0, nil
	}
	if av.Kind() == reflect.Float32 || av.Kind() == reflect.Float64 {
		x, y := av.Float(), bv.Convert(av.Type()).Float()
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("values of type %T are not ordered", a)
}

func isSigned(k reflect.Kind) bool { return k >= reflect.Int && k <= reflect.Int64 }
func isUnsigned(k reflect.Kind) bool {
	return k == reflect.Uint || k == reflect.Uint8 || k == reflect.Uint16 || k == reflect.Uint32 || k == reflect.Uint64 || k == reflect.Uintptr
}
func toInt64(v any) int64 {
	rv := indirect(v)
	if !rv.IsValid() {
		return 0
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return int64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return int64(rv.Float())
	}
	return 0
}
func toFloat64(v any) float64 {
	rv := indirect(v)
	if !rv.IsValid() {
		return 0
	}
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint())
	}
	return 0
}

func toReflect(value any, target reflect.Type) (reflect.Value, error) {
	return toReflectContext(value, target, context.Background())
}

func toReflectContext(value any, target reflect.Type, ctx context.Context) (reflect.Value, error) {
	if target == nil {
		return reflect.Value{}, fmt.Errorf("unknown target type")
	}
	if value == nil {
		switch target.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
			return reflect.Zero(target), nil
		}
		return reflect.Value{}, fmt.Errorf("nil is not assignable to %s", target)
	}
	rv := indirect(value)
	if !rv.IsValid() {
		return reflect.Value{}, fmt.Errorf("invalid value")
	}
	if rv.Type().AssignableTo(target) {
		return rv, nil
	}
	if target.Kind() == reflect.Interface && rv.Type().Implements(target) {
		return rv, nil
	}
	if rv.Type().ConvertibleTo(target) {
		return rv.Convert(target), nil
	}
	if target.Kind() == reflect.Func {
		if scriptFn, ok := value.(*Function); ok {
			callback := reflect.MakeFunc(target, func(in []reflect.Value) []reflect.Value {
				args := make([]any, len(in))
				for i, v := range in {
					args[i] = v.Interface()
				}
				if target.IsVariadic() && len(args) > 0 {
					last := indirect(args[len(args)-1])
					if last.IsValid() && last.Kind() == reflect.Slice {
						expanded := append([]any(nil), args[:len(args)-1]...)
						for i := 0; i < last.Len(); i++ {
							expanded = append(expanded, last.Index(i).Interface())
						}
						args = expanded
					}
				}
				result, callErr := scriptFn.rt.callFunction(ctx, scriptFn, args)
				results := make([]reflect.Value, target.NumOut())
				for i := range results {
					results[i] = reflect.Zero(target.Out(i))
				}
				var values []any
				if many, ok := result.(tuple); ok {
					values = []any(many)
				} else if result != nil {
					values = []any{result}
				}
				if callErr != nil {
					if len(results) > 0 && target.Out(len(results)-1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
						results[len(results)-1] = reflect.ValueOf(callErr)
						return results
					}
					panic(callErr)
				}
				for i := range values {
					if i < len(results) {
						converted, err := toReflectContext(values[i], target.Out(i), ctx)
						if err != nil {
							panic(err)
						}
						results[i] = converted
					}
				}
				return results
			})
			return callback, nil
		}
	}
	if target.Kind() == reflect.Slice && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		out := reflect.MakeSlice(target, rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			v, err := toReflectContext(rv.Index(i).Interface(), target.Elem(), ctx)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(v)
		}
		return out, nil
	}
	if target.Kind() == reflect.Map && rv.Kind() == reflect.Map {
		out := reflect.MakeMapWithSize(target, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k, err := toReflectContext(iter.Key().Interface(), target.Key(), ctx)
			if err != nil {
				return reflect.Value{}, err
			}
			v, err := toReflectContext(iter.Value().Interface(), target.Elem(), ctx)
			if err != nil {
				return reflect.Value{}, err
			}
			out.SetMapIndex(k, v)
		}
		return out, nil
	}
	if target.Kind() == reflect.Struct && rv.Kind() == reflect.Map {
		out := reflect.New(target).Elem()
		iter := rv.MapRange()
		for iter.Next() {
			key := fmt.Sprint(iter.Key().Interface())
			field := out.FieldByName(key)
			if !field.IsValid() || !field.CanSet() {
				continue
			}
			v, err := toReflectContext(iter.Value().Interface(), field.Type(), ctx)
			if err != nil {
				return reflect.Value{}, err
			}
			field.Set(v)
		}
		return out, nil
	}
	return reflect.Value{}, fmt.Errorf("cannot use %s as %s", rv.Type(), target)
}
