package eval

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
)

type frame struct {
	rt  *Runtime
	env *env
	ctx context.Context
}

func (f *frame) execBlock(block *ast.BlockStmt) (flow, error) {
	if block == nil {
		return flow{}, nil
	}
	local := newEnv(f.env)
	child := *f
	child.env = local
	for _, stmt := range block.List {
		if err := child.rt.tick(child.ctx); err != nil {
			return flow{}, err
		}
		out, err := child.execStmt(stmt)
		if err != nil {
			return flow{}, err
		}
		if out.kind != flowNormal {
			return out, nil
		}
	}
	return flow{}, nil
}

func (f *frame) execStmt(stmt ast.Stmt) (flow, error) {
	if err := f.rt.tick(f.ctx); err != nil {
		return flow{}, err
	}
	switch s := stmt.(type) {
	case *ast.EmptyStmt:
		return flow{}, nil
	case *ast.BlockStmt:
		return f.execBlock(s)
	case *ast.ExprStmt:
		_, err := f.rt.evalExpr(f.ctx, s.X, f.env)
		return flow{}, err
	case *ast.DeclStmt:
		return flow{}, f.execDecl(s.Decl)
	case *ast.AssignStmt:
		return flow{}, f.execAssign(s)
	case *ast.IncDecStmt:
		v, err := f.rt.evalExpr(f.ctx, s.X, f.env)
		if err != nil {
			return flow{}, err
		}
		n, err := binaryValue(token.ADD, v, int64(1))
		if s.Tok == token.DEC {
			n, err = binaryValue(token.SUB, v, int64(1))
		}
		if err != nil {
			return flow{}, err
		}
		return flow{}, f.assign(s.X, n, false)
	case *ast.ReturnStmt:
		values, err := f.evalExprList(s.Results)
		if err != nil {
			return flow{}, err
		}
		return flow{kind: flowReturn, values: values}, nil
	case *ast.IfStmt:
		if s.Init != nil {
			if _, err := f.execStmt(s.Init); err != nil {
				return flow{}, err
			}
		}
		cond, err := f.rt.evalExpr(f.ctx, s.Cond, f.env)
		if err != nil {
			return flow{}, err
		}
		if truthy(cond) {
			return f.execBlock(s.Body)
		}
		if s.Else != nil {
			return f.execStmt(s.Else)
		}
		return flow{}, nil
	case *ast.ForStmt:
		return f.execFor(s)
	case *ast.RangeStmt:
		return f.execRange(s)
	case *ast.BranchStmt:
		switch s.Tok {
		case token.BREAK:
			return flow{kind: flowBreak}, nil
		case token.CONTINUE:
			return flow{kind: flowContinue}, nil
		case token.FALLTHROUGH:
			return flow{}, fmt.Errorf("fallthrough is not supported by the current evaluator")
		}
		return flow{}, fmt.Errorf("unsupported branch %s", s.Tok)
	case *ast.SwitchStmt:
		return f.execSwitch(s)
	case *ast.LabeledStmt:
		return f.execStmt(s.Stmt)
	default:
		return flow{}, fmt.Errorf("%s: statement %T is not yet supported by the evaluator", f.rt.checked.Program.Fset.Position(stmt.Pos()), stmt)
	}
}

func (f *frame) execDecl(decl ast.Decl) error {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("unsupported declaration %T", decl)
	}
	switch gd.Tok {
	case token.VAR, token.CONST:
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			var values []any
			if len(vs.Values) > 0 {
				var err error
				values, err = f.evalExprList(vs.Values)
				if err != nil {
					return err
				}
			}
			for i, name := range vs.Names {
				var value any
				if i < len(values) {
					value = values[i]
				}
				if gd.Tok == token.VAR {
					if obj, ok := f.rt.checked.Info.Defs[name].(*types.Var); ok {
						if i < len(values) {
							var err error
							value, err = convertToType(value, obj.Type())
							if err != nil {
								return err
							}
						} else if t := reflectTypeFor(obj.Type()); t != nil {
							value = reflect.Zero(t).Interface()
						}
					}
				}
				if gd.Tok == token.CONST {
					if obj, ok := f.rt.checked.Info.Defs[name].(*types.Const); ok {
						var err error
						value, err = constantToValue(obj.Val(), obj.Type())
						if err != nil {
							return err
						}
					}
				}
				f.env.define(name.Name, value)
			}
		}
	case token.TYPE:
		// Type declarations are fully checked by go/types. Runtime composite
		// values are represented by their underlying Go-compatible values.
	default:
		return fmt.Errorf("unsupported declaration token %s", gd.Tok)
	}
	return nil
}

func (f *frame) execAssign(s *ast.AssignStmt) error {
	values, err := f.evalExprList(s.Rhs)
	if err != nil {
		return err
	}
	if len(values) != len(s.Lhs) {
		return fmt.Errorf("assignment has %d targets but %d values", len(s.Lhs), len(values))
	}
	for i, lhs := range s.Lhs {
		value := values[i]
		if op, ok := compoundOperator(s.Tok); ok {
			current, err := f.rt.evalExpr(f.ctx, lhs, f.env)
			if err != nil {
				return err
			}
			value, err = binaryValue(op, current, value)
			if err != nil {
				return err
			}
		}
		if err := f.assign(lhs, value, s.Tok == token.DEFINE); err != nil {
			return err
		}
	}
	return nil
}

func compoundOperator(tok token.Token) (token.Token, bool) {
	switch tok {
	case token.ADD_ASSIGN:
		return token.ADD, true
	case token.SUB_ASSIGN:
		return token.SUB, true
	case token.MUL_ASSIGN:
		return token.MUL, true
	case token.QUO_ASSIGN:
		return token.QUO, true
	case token.REM_ASSIGN:
		return token.REM, true
	case token.AND_ASSIGN:
		return token.AND, true
	case token.OR_ASSIGN:
		return token.OR, true
	case token.XOR_ASSIGN:
		return token.XOR, true
	case token.SHL_ASSIGN:
		return token.SHL, true
	case token.SHR_ASSIGN:
		return token.SHR, true
	case token.AND_NOT_ASSIGN:
		return token.AND_NOT, true
	default:
		return token.ILLEGAL, false
	}
}

func (f *frame) assign(lhs ast.Expr, value any, define bool) error {
	switch x := lhs.(type) {
	case *ast.Ident:
		if x.Name == "_" {
			return nil
		}
		typ := f.rt.checked.Info.TypeOf(x)
		if define {
			if obj := f.rt.checked.Info.Defs[x]; obj != nil {
				typ = obj.Type()
			}
		}
		if typ != nil {
			converted, err := convertToType(value, typ)
			if err != nil {
				return fmt.Errorf("assign %s: %w", x.Name, err)
			}
			value = converted
		}
		if define {
			f.env.define(x.Name, value)
			return nil
		}
		return f.env.assign(x.Name, value)
	case *ast.IndexExpr:
		container, err := f.rt.evalExpr(f.ctx, x.X, f.env)
		if err != nil {
			return err
		}
		index, err := f.rt.evalExpr(f.ctx, x.Index, f.env)
		if err != nil {
			return err
		}
		return assignIndex(container, index, value)
	case *ast.SelectorExpr:
		base, err := f.rt.evalExpr(f.ctx, x.X, f.env)
		if err != nil {
			return err
		}
		rv := indirect(base)
		if !rv.IsValid() {
			return fmt.Errorf("cannot assign field %s on nil value", x.Sel.Name)
		}
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return fmt.Errorf("nil pointer")
			}
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return fmt.Errorf("cannot assign selector on %T", base)
		}
		field := rv.FieldByName(x.Sel.Name)
		if !field.IsValid() || !field.CanSet() {
			return fmt.Errorf("field %s is not settable", x.Sel.Name)
		}
		v, err := toReflect(value, field.Type())
		if err != nil {
			return err
		}
		field.Set(v)
		return nil
	case *ast.StarExpr:
		ptr, err := f.rt.evalExpr(f.ctx, x.X, f.env)
		if err != nil {
			return err
		}
		rv := indirect(ptr)
		if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() || !rv.Elem().CanSet() {
			return fmt.Errorf("cannot assign through pointer")
		}
		v, err := toReflect(value, rv.Elem().Type())
		if err != nil {
			return err
		}
		rv.Elem().Set(v)
		return nil
	default:
		return fmt.Errorf("assignment target %T is not supported", lhs)
	}
}

func (f *frame) execFor(s *ast.ForStmt) (flow, error) {
	loop := newEnv(f.env)
	child := *f
	child.env = loop
	if s.Init != nil {
		if _, err := child.execStmt(s.Init); err != nil {
			return flow{}, err
		}
	}
	for {
		if err := f.rt.tick(f.ctx); err != nil {
			return flow{}, err
		}
		if s.Cond != nil {
			cond, err := f.rt.evalExpr(f.ctx, s.Cond, child.env)
			if err != nil {
				return flow{}, err
			}
			if !truthy(cond) {
				break
			}
		}
		out, err := child.execBlock(s.Body)
		if err != nil {
			return flow{}, err
		}
		if out.kind == flowReturn {
			return out, nil
		}
		if out.kind == flowBreak {
			break
		}
		if s.Post != nil {
			if _, err := child.execStmt(s.Post); err != nil {
				return flow{}, err
			}
		}
	}
	return flow{}, nil
}

func (f *frame) execRange(s *ast.RangeStmt) (flow, error) {
	value, err := f.rt.evalExpr(f.ctx, s.X, f.env)
	if err != nil {
		return flow{}, err
	}
	iter := make([][2]any, 0)
	rv := indirect(value)
	if !rv.IsValid() {
		return flow{}, fmt.Errorf("cannot range over nil")
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Slice:
		for i := 0; i < rv.Len(); i++ {
			iter = append(iter, [2]any{int64(i), rv.Index(i).Interface()})
		}
	case reflect.String:
		// Go's string range yields UTF-8 byte offsets and rune values, not
		// rune ordinals. A native range also matches Go's invalid UTF-8 behavior.
		for offset, r := range rv.String() {
			iter = append(iter, [2]any{int64(offset), r})
		}
	case reflect.Map:
		mi := rv.MapRange()
		for mi.Next() {
			iter = append(iter, [2]any{mi.Key().Interface(), mi.Value().Interface()})
		}
	default:
		return flow{}, fmt.Errorf("cannot range over %T", value)
	}
	for _, pair := range iter {
		loop := newEnv(f.env)
		child := *f
		child.env = loop
		if s.Key != nil {
			if err := child.assign(s.Key, pair[0], s.Tok == token.DEFINE); err != nil {
				return flow{}, err
			}
		}
		if s.Value != nil {
			if err := child.assign(s.Value, pair[1], s.Tok == token.DEFINE); err != nil {
				return flow{}, err
			}
		}
		out, err := child.execBlock(s.Body)
		if err != nil {
			return flow{}, err
		}
		if out.kind == flowReturn {
			return out, nil
		}
		if out.kind == flowBreak {
			break
		}
	}
	return flow{}, nil
}

func (f *frame) execSwitch(s *ast.SwitchStmt) (flow, error) {
	switchEnv := newEnv(f.env)
	child := *f
	child.env = switchEnv
	if s.Init != nil {
		if _, err := child.execStmt(s.Init); err != nil {
			return flow{}, err
		}
	}
	var tag any = true
	var err error
	if s.Tag != nil {
		tag, err = child.rt.evalExpr(f.ctx, s.Tag, switchEnv)
		if err != nil {
			return flow{}, err
		}
	}
	var fallback *ast.CaseClause
	for _, stmt := range s.Body.List {
		clause := stmt.(*ast.CaseClause)
		if clause.List == nil {
			fallback = clause
			continue
		}
		for _, expr := range clause.List {
			v, err := child.rt.evalExpr(f.ctx, expr, switchEnv)
			if err != nil {
				return flow{}, err
			}
			if equal(tag, v) {
				return child.execCase(clause)
			}
		}
	}
	if fallback != nil {
		return child.execCase(fallback)
	}
	return flow{}, nil
}

func (f *frame) execCase(c *ast.CaseClause) (flow, error) {
	local := newEnv(f.env)
	child := *f
	child.env = local
	for _, stmt := range c.Body {
		out, err := child.execStmt(stmt)
		if err != nil {
			return flow{}, err
		}
		if out.kind != flowNormal {
			if out.kind == flowBreak {
				return flow{}, nil
			}
			return out, nil
		}
	}
	return flow{}, nil
}

func (f *frame) evalExprList(exprs []ast.Expr) ([]any, error) {
	values := make([]any, 0, len(exprs))
	for _, expr := range exprs {
		v, err := f.rt.evalExpr(f.ctx, expr, f.env)
		if err != nil {
			return nil, err
		}
		if many, ok := v.(tuple); ok {
			values = append(values, many...)
		} else {
			values = append(values, v)
		}
	}
	return values, nil
}
