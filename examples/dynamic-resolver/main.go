// Example: resolve arbitrary import paths from an application-owned registry.
// No Go module lookup, filesystem probing, or network access is performed by
// Golark. The registry can be populated by the embedding application at
// startup from any trusted source.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ep0ll/golark-go"
)

type sourceRegistry map[string]golark.PackageSource

func (r sourceRegistry) Resolve(ctx context.Context, importPath string) (golark.PackageSource, error) {
	if err := ctx.Err(); err != nil {
		return golark.PackageSource{}, err
	}
	pkg, ok := r[importPath]
	if !ok {
		return golark.PackageSource{}, golark.ErrPackageNotFound
	}
	return pkg, nil
}

func main() {
	// The application decides which import paths exist and what source they
	// resolve to. This could be backed by a database, tenant configuration,
	// an embedded bundle, or an in-memory cache.
	registry := sourceRegistry{
		"acme.local/textutil": {
			Files: map[string][]byte{
				"textutil.go": []byte(`package textutil

func Normalize(s string) string {
	return s
}
`),
			},
		},
		"acme.local/labels": {
			Files: map[string][]byte{
				"labels.go": []byte(`package labels

import "acme.local/textutil"

func Build(name string) string {
	return textutil.Normalize("service:" + name)
}
`),
			},
		},
	}

	// This root imports labels. The loader discovers labels' transitive
	// textutil import through the same resolver and shared package graph.
	module, err := golark.Exec(context.Background(), "config.go", []byte(`package config

import "acme.local/labels"

func Label(name string) string {
	return labels.Build(name)
}
`), golark.Options{
		Resolver: registry,
	})
	if err != nil {
		panic(err)
	}

	result, err := module.Call(context.Background(), "Label", "gateway")
	if err != nil {
		panic(err)
	}
	fmt.Println(result[0]) // service:gateway

	// A resolver can be layered: check a tenant-specific registry first,
	// then a shared registry. Only the embedding application defines policy.
	_ = golark.ResolverFunc(func(ctx context.Context, path string) (golark.PackageSource, error) {
		if pkg, ok := registry[path]; ok {
			return pkg, nil
		}
		if strings.HasPrefix(path, "tenant.local/") {
			return golark.PackageSource{}, fmt.Errorf("tenant package %q is not registered", path)
		}
		return golark.PackageSource{}, golark.ErrPackageNotFound
	})
}
