# Golark

Golark is an embeddable, statically typed Go configuration language and runtime.
It uses Go source syntax rather than inventing a second grammar, and uses the Go
standard library to parse, type-check, inspect, and format programs.

> **Status: early foundation / experimental.** The initial implementation
> provides the public embedding API, a Go parser and type-checking pipeline,
> typed host bindings, package namespaces, and an AST evaluator for common
> configuration-oriented Go constructs. It is not yet a drop-in implementation
> of every Go runtime feature. Unsupported AST nodes fail explicitly rather
> than being silently ignored.

## Design goals

* Preserve Go's static type system and familiar Go syntax.
* Reuse `go/parser`, `go/ast`, `go/token`, `go/types`, `go/constant`, and
  `go/format`; do not fork Go's lexer or parser.
* Embed scripts in a Go process and expose host functions and values with
  checked signatures.
* Keep parsing, type checking, evaluation, module initialization, and host
  integration separate.
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

## Imports

A script can use ordinary Go import declarations. Static package information is
resolved by `Options.Importer` (default: `go/importer.Default`). Runtime package
values are supplied by exact import path:

```go
type stringsAPI struct {
    TrimSpace func(string) string
}

opts := golark.Options{
    Imports: map[string]map[string]golark.Binding{
        "strings": {
            "TrimSpace": golark.Bind(strings.TrimSpace),
        },
    },
}
```

In production, a host commonly supplies a narrow package facade rather than
exposing a large standard-library or application package wholesale.

## Architecture

```text
source
  │
  ▼
go/parser ──► go/ast + go/token
  │
  ▼
go/types ───► typed package + Info (Defs, Uses, Types, Selections, ...)
  │
  ▼
Golark evaluator ──► module globals / script functions / host calls
  │
  ▼
embedded Go application
```

See [docs/architecture.md](docs/architecture.md) for package boundaries,
execution semantics, security considerations, and the planned compatibility
work.

## Current evaluator coverage

The evaluator currently handles package-level functions and variables,
constants, typed host bindings, function calls, basic literals, arithmetic and
comparisons, selectors, indexing and slicing, slice/map/array/struct composite
literals, conversions, `if`, `for`, `range`, `switch`, `return`, assignments,
short declarations, and common built-ins (`len`, `cap`, `append`, `make`,
`new`, `copy`, `delete`, `clear`, `min`, `max`, `panic`, `print`, `println`).
The whole file is still parsed and statically type-checked as Go, but some
valid Go constructs are not yet executable by the evaluator (for example,
`select`, `go`, `defer`, labels/goto, generic instantiations, and some complex
method/interface cases).

## Non-goals

Golark is not Python, Starlark, or a source-to-source Go transpiler. `func`,
Go types, import declarations, and Go expressions retain Go spelling and type
rules. A Go source file is the language surface; the runtime is the embeddable
configuration/scripting layer.

## License

MIT. See [LICENSE](LICENSE).
