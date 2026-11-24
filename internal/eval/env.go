package eval

import (
	"fmt"
	"reflect"
)

type cell struct{ value any }

type env struct {
	parent *env
	values map[string]*cell
}

func newEnv(parent *env) *env { return &env{parent: parent, values: make(map[string]*cell)} }

func (e *env) define(name string, value any) {
	if name == "_" {
		return
	}
	e.values[name] = &cell{value: value}
}

func (e *env) find(name string) (*cell, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		if c, ok := cur.values[name]; ok {
			return c, true
		}
	}
	return nil, false
}

func (e *env) get(name string) (any, bool) {
	c, ok := e.find(name)
	if !ok {
		return nil, false
	}
	return c.value, true
}

func (e *env) assign(name string, value any) error {
	c, ok := e.find(name)
	if !ok {
		return fmt.Errorf("undefined variable %q", name)
	}
	c.value = value
	return nil
}

func indirect(v any) reflect.Value {
	if rv, ok := v.(reflect.Value); ok {
		return rv
	}
	if v == nil {
		return reflect.Value{}
	}
	return reflect.ValueOf(v)
}
