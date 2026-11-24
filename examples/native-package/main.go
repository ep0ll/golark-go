// Example: expose a small native package with explicit static declarations
// and runtime implementations. The script sees only the API registered here.
package main

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"github.com/ep0ll/golark-go"
)

type nativeImporter struct {
	packages map[string]*types.Package
}

func (i nativeImporter) Import(path string) (*types.Package, error) {
	pkg, ok := i.packages[path]
	if !ok {
		return nil, golark.ErrPackageNotFound
	}
	return pkg, nil
}

func stringFunction(pkg *types.Package, name string) *types.Func {
	stringType := types.Typ[types.String]
	params := types.NewTuple(types.NewVar(token.NoPos, pkg, "value", stringType))
	results := types.NewTuple(types.NewVar(token.NoPos, pkg, "", stringType))
	signature := types.NewSignatureType(nil, nil, nil, params, results, false)
	return types.NewFunc(token.NoPos, pkg, name, signature)
}

func main() {
	const importPath = "acme.local/slug"

	// Build static package metadata. This is what go/types uses to validate
	// selectors, argument types, and return types in the script.
	slug := types.NewPackage(importPath, "slug")
	slug.Scope().Insert(stringFunction(slug, "Kebab"))
	slug.MarkComplete()

	// Register the corresponding runtime values separately. Keep this list
	// deliberately small: a native package grants capabilities to the script.
	opts := golark.Options{
		Importer: nativeImporter{packages: map[string]*types.Package{
			importPath: slug,
		}},
		Imports: map[string]map[string]golark.Binding{
			importPath: {
				"Kebab": golark.Bind(func(value string) string {
					return strings.NewReplacer(" ", "-").Replace(strings.ToLower(value))
				}),
			},
		},
	}

	module, err := golark.Exec(context.Background(), "config.go", []byte(`package config

import "acme.local/slug"

func ResourceName(name string) string {
	return slug.Kebab(name)
}
`), opts)
	if err != nil {
		panic(err)
	}

	result, err := module.Call(context.Background(), "ResourceName", "Build Worker")
	if err != nil {
		panic(err)
	}
	fmt.Println(result[0]) // build-worker
}
