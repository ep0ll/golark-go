// Example: source imports and native imports in one package graph.
// A source package may itself import a native package; both are resolved by
// one Exec operation.
package main

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/ep0ll/golark-go"
)

type importer map[string]*types.Package

func (i importer) Import(path string) (*types.Package, error) {
	pkg, ok := i[path]
	if !ok {
		return nil, golark.ErrPackageNotFound
	}
	return pkg, nil
}

func main() {
	const nativePath = "acme.local/strings"

	native := types.NewPackage(nativePath, "strings")
	params := types.NewTuple(types.NewVar(token.NoPos, native, "s", types.Typ[types.String]))
	results := types.NewTuple(types.NewVar(token.NoPos, native, "", types.Typ[types.String]))
	native.Scope().Insert(types.NewFunc(token.NoPos, native, "Upper", types.NewSignatureType(nil, nil, nil, params, results, false)))
	native.MarkComplete()

	resolver := golark.ResolverFunc(func(ctx context.Context, path string) (golark.PackageSource, error) {
		if err := ctx.Err(); err != nil {
			return golark.PackageSource{}, err
		}
		if path != "acme.local/greeting" {
			return golark.PackageSource{}, golark.ErrPackageNotFound
		}
		return golark.PackageSource{Files: map[string][]byte{
			"greeting.go": []byte(`package greeting
import "acme.local/strings"
func Welcome(name string) string {
	return strings.Upper("hello, " + name)
}
`),
		}}, nil
	})

	module, err := golark.Exec(context.Background(), "config.go", []byte(`package config
import "acme.local/greeting"
func Message() string { return greeting.Welcome("world") }
`), golark.Options{
		Resolver: resolver,
		Importer: importer{nativePath: native},
		Imports: map[string]map[string]golark.Binding{
			nativePath: {
				"Upper": golark.Bind(strings.ToUpper),
			},
		},
	})
	if err != nil {
		panic(err)
	}
	result, err := module.Call(context.Background(), "Message")
	if err != nil {
		panic(err)
	}
	fmt.Println(result[0]) // HELLO, WORLD
}
