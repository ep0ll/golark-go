# Golark

Golark is an embeddable, statically typed Go configuration language and runtime.
It uses Go source syntax rather than inventing a second grammar, and uses the Go
standard library to parse, type-check, inspect, and format programs.

> **Status: early foundation / experimental.** The implementation provides a
> public embedding API, Go parsing and type checking, in-memory package graph
> loading, typed native bindings, and an AST evaluator for common
> configuration-oriented Go constructs. It is not yet a drop-in implementation
> of every Go runtime feature. Unsupported AST nodes fail explicitly rather
> than being silently ignored.

## Design goals

* Preserve Go's static type system and familiar Go syntax.
* Reuse `go/parser`, `go/ast`, `go/token`, `go/types`, `go/constant`, and
  `go/format`; do not fork Go's lexer or parser.
* Embed scripts in a Go process and expose typed native functions and values.
* Let the embedding application own source resolution for non-native imports.
* Load and type-check the complete source package graph in one execution,
  preserving one `*types.Package` identity per canonical import path.
* Keep parsing, package loading, type checking, evaluation, and host integration
  separate.
* Make execution deterministic where Go permits it, cancellable through
  `context.Context`, and bounded by an instruction/step budget.
* Grow toward the complete Go language without pretending the standard library
  already contains an interpreter. Go's parser and type checker do not execute
  ASTs; Golark owns that runtime layer.

## Quick start

```go
package main

import (
    "context"
    "fmt"

    "github.com/ep0ll/golark-go"
)

func main() {
    source := []byte(`package config

func Greeting(name string) string {
    return "hello, " + name
}
`)

    module, err := golark.Exec(context.Background(), "config.go", source, golark.Options{})
    if err != nil {
        panic(err)
    }

    result, err := module.Call(context.Background(), "Greeting", "world")
    if err != nil {
        panic(err)
    }
    fmt.Println(result[0]) // hello, world
}
```

## Exposing host functions

Host functions are statically visible to the Go type checker and callable by
scripts at runtime:

```go
opts := golark.Options{
    Predeclared: map[string]golark.Binding{
        "add": golark.Bind(func(a, b int) int { return a + b }),
        "prefix": golark.Bind("build-"),
    },
}

module, err := golark.Exec(ctx, "config.go", []byte(`package config
func Name(n string) string { return prefix + n }
func Sum() int { return add(20, 22) }
`), opts)
```

The checker rejects wrong argument and assignment types before evaluation.
For unusual host types, provide an explicit `Binding{Value: value, Type: t}`.

## Source imports and package graphs

Imports use ordinary Go import declarations. The embedding application supplies
the source for external packages through `Options.Resolver`:

```go
resolver := golark.ResolverFunc(func(
    ctx context.Context,
    importPath string,
) (golark.PackageSource, error) {
    switch importPath {
    case "example.com/mathx":
        return golark.PackageSource{
            ImportPath: importPath,
            Files: map[string][]byte{
                "mathx.go": []byte(`package mathx
func Double(n int) int { return n * 2 }
`),
            },
        }, nil
    default:
        return golark.PackageSource{}, golark.ErrPackageNotFound
    }
})

module, err := golark.Exec(ctx, "config.go", []byte(`package config
import "example.com/mathx"
func Answer() int { return mathx.Double(21) }
`), golark.Options{Resolver: resolver})
```

One `Exec` loads the root package and every source-backed transitive import.
Golark calls the resolver by import path, caches the result for that operation,
and returns the same `*types.Package` whenever an import path is encountered
again. The package graph is type-checked as one graph and initialized in
dependency-first order. Callers do not need to invoke `Exec` recursively or
assemble separately checked package modules.

`PackageSource.ImportPath` may be empty, in which case Golark uses the exact
requested import path. Otherwise it must equal the requested path. The
`Files` map contains source bytes; its keys are diagnostic filenames, not
filesystem paths. Golark does not use Go Modules, `go list`, the network, or
the filesystem to discover imported source packages.

## Native packages

A package implemented by the embedding Go application can be exposed as a
native package instead of interpreted source. Native packages have two
separate parts:

* `Options.Importer` provides their static exported declarations as
  `*types.Package` values.
* `Options.Imports` provides the matching runtime values, keyed by exact
  import path and exported identifier.

For example, a host may use a curated facade for `strings`:

```go
import "go/importer"
import stdstrings "strings"

opts := golark.Options{
    Importer: importer.Default(), // type information only
    Imports: map[string]map[string]golark.Binding{
        "strings": {
            "TrimSpace": golark.Bind(stdstrings.TrimSpace),
        },
    },
}
```

A source package returned by `Resolver` takes precedence. Returning
`golark.ErrPackageNotFound` tells Golark to try the native `Importer`.
When neither resolves an import, type checking fails. Golark does not
automatically discover application packages or download modules.

A native facade should expose only the capabilities scripts are allowed to
use. Static typing does not make a native function safe: the host remains
responsible for the behavior of every function and value it registers.

## Runnable examples

The repository includes focused, standalone embedding examples. Run any of them
from the repository root with `go run ./examples/<name>`.

| Example | Demonstrates |
| --- | --- |
| [basic](examples/basic/main.go) | Parse, execute, and call a root-package function. |
| [dynamic-resolver](examples/dynamic-resolver/main.go) | Resolve import paths dynamically from an application-owned registry, including transitive imports. |
| [native-package](examples/native-package/main.go) | Register a native package with explicit `go/types` declarations and matching runtime bindings. |
| [mixed-packages](examples/mixed-packages/main.go) | Combine source-backed packages and native packages in one import graph, including a native dependency imported by a source package. |

### Dynamic package resolution

A resolver can be backed by a map, database, tenant-specific registry, embedded
asset bundle, cache, or another application-controlled source. It receives the
exact import path requested by the script and returns source bytes:

```go
resolver := golark.ResolverFunc(func(
    ctx context.Context,
    importPath string,
) (golark.PackageSource, error) {
    if err := ctx.Err(); err != nil {
        return golark.PackageSource{}, err
    }

    source, ok := registry[importPath]
    if !ok {
        return golark.PackageSource{}, golark.ErrPackageNotFound
    }
    return source, nil
})
```

"Unknown package" is not an automatic download instruction. It means the
resolver receives an import path that is not known to the root source, and the
embedding application decides whether that path is registered, authorized,
and available. Returning `ErrPackageNotFound` permits native-importer fallback;
returning any other error stops resolution. A resolver may compose multiple
registries (for example tenant packages first, then shared packages), but it
should enforce its own allowlist and trust policy.

### Native package registration

Native packages have a static half and a runtime half:

1. The `types.Importer` returns a completed `*types.Package` describing the
   exported names and exact signatures visible to `go/types`.
2. `Options.Imports` maps those same exported names to runtime
   `golark.Binding` values.

The example builds the package metadata with `go/types.NewPackage`,
`types.NewFunc`, and `types.NewSignatureType`. This is useful when the host
wants to expose a narrow facade over a much larger Go package. Do not expose a
package's full API by default: each registered native function is a host
capability and can perform whatever operations its implementation allows.

The combined example demonstrates the resolution order: Golark asks the
source resolver first; if it returns `ErrPackageNotFound`, Golark asks the
native importer. The same rule applies to transitive imports. The native
importer should return stable package objects for each import path.

## Architecture

```text
root source + in-memory PackageResolver
                 │
                 ▼
        shared package loader/cache
                 │
                 ▼
     go/parser ──► go/ast + go/token
                 │
                 ▼
       go/types ◄── types.Importer / native types
                 │
                 ▼
    dependency-first package initialization
                 │
                 ▼
      Golark evaluator + native bindings
                 │
                 ▼
           embedded Go application
```

See [docs/architecture.md](docs/architecture.md) for package boundaries,
package identity, execution semantics, and security considerations.

## Current evaluator coverage

The evaluator currently handles package-level functions and variables,
constants, typed host bindings, function calls, basic literals, arithmetic and
comparisons, selectors, indexing and slicing, slice/map/array/struct composite
literals, conversions, `if`, `for`, `range`, `switch`, `return`, assignments,
short declarations, and common built-ins (`len`, `cap`, `append`, `make`,
`new`, `copy`, `delete`, `clear`, `min`, `max`, `panic`, `print`, `println`).
The whole package graph is statically type-checked, but some valid Go constructs
are not yet executable by the evaluator (for example, `select`, `go`,
`defer`, labels/goto, generic instantiations, and some complex
method/interface cases).

## Non-goals

Golark is not Python, Starlark, or a source-to-source Go transpiler. `func`,
Go types, import declarations, and Go expressions retain Go spelling and type
rules. A Go source file is the language surface; the runtime is the embeddable
configuration/scripting layer.

## License

MIT. See [LICENSE](LICENSE).
