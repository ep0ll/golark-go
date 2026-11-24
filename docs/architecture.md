# Golark architecture

## 1. Go front end, interpreted execution

Golark uses the Go standard library front end instead of maintaining a second
language grammar. `go/parser` builds the AST, `go/types` validates static types
and resolves imported declarations, `go/constant` preserves compile-time
constant semantics, and `go/format` provides canonical formatting.

The Go standard library does not evaluate Go ASTs. `internal/eval` is therefore
a separate runtime implementation. Successful parsing and type checking do
not imply that every Go AST node is already executable by Golark.

## 2. Package boundaries

* `internal/syntax`: thin, policy-free wrappers around `go/parser` and
  `go/format`. No custom tokens or grammar.
* `internal/loader`: resolves imported source through the application's
  resolver, builds the transitive package graph, caches package identities by
  import path, and supplies a shared `types.ImporterFrom`.
* `internal/checker`: inserts typed host bindings and invokes `go/types` for
  each package using the loader's shared importer.
* `internal/eval`: runtime environments, module initialization, function
  invocation, reflection-based native calls, expression evaluation, statement
  execution, cancellation, and step accounting.
* `golark` (module root): public embedding API. It wires graph loading,
  dependency-first initialization, and the public module handle together.
* `cmd/golark`: command-line entry point.

The dependency direction is deliberately one-way:

```text
golark API -> loader -> checker -> Go standard library
     |                        ^
     +-> evaluator -----------+
```

The loader depends on the checker, not the reverse. The checker sees only the
`types.Importer` interface and does not know where imported source comes from.

## 3. In-memory package resolution and identity

The embedding application owns source resolution:

```go
type PackageSource struct {
    ImportPath string
    Files      map[string][]byte
}

type PackageResolver interface {
    Resolve(context.Context, string) (PackageSource, error)
}
```

The resolver is called whenever the type checker encounters an import.
Golark parses the returned files in memory and checks the imported package with
the same importer instance used for the root package. The importer implements
both `types.Importer` and `types.ImporterFrom`; the latter preserves Go's
importer context even though source lookup is keyed by canonical import path.

For one `Check` or `Exec` operation, the loader maintains a per-path state
cache. A source-backed package is resolved and type-checked once. Repeated
imports, including imports reached through different transitive paths, receive
the same `*types.Package`. This is essential: named types are identified by
their declaring package, and independently type-checking two copies of one
import path can make otherwise identical types incompatible.

The resolver's returned `ImportPath`, when supplied, must match the requested
path. `Files` is a map of source bytes; filenames are used only for source
positions and diagnostics. The loader does not call `go list`, use Go Modules,
download packages, or search directories for imported source.

A resolver returns `ErrPackageNotFound` when it does not own a path. Golark
then tries `Options.Importer`, if configured. This importer is for native or
type-only packages. Native packages are also supplied with runtime values via
`Options.Imports`. The importer result is cached by path for the same graph,
so a native package also has one stable static package identity.

Source packages and native packages are intentionally distinct:

1. **Source package**: returned by the resolver, parsed, checked, interpreted,
   and initialized with its dependencies.
2. **Native package**: returned by the application importer, checked against
   its exported type declarations, and bound to explicit runtime values from
   the host. Golark does not execute or introspect its implementation.

Source resolution takes precedence over native import fallback. If neither
can resolve an import, checking fails immediately.

## 4. Runtime package graph and initialization

The loader records source-backed packages in dependency-first order. Execution
initializes each package exactly once for that module graph, after all of its
source dependencies have initialized. This means package-level variables and
`init` functions in a dependency are ready before an importing package uses
them.

After a source package is initialized, Golark exposes its exported package
values to importing modules. Those values retain the function's owning runtime:
a call to a function in an imported package must evaluate its AST with that
package's own `types.Info`, global environment, and step counter. Package
globals and functions are not copied into independent runtime instances.

Native runtime values are merged into the same import namespace. For a native
package, the host must ensure the names and runtime values in `Options.Imports`
match the static API returned by `Options.Importer`.

## 5. Static host bindings

An application may seed identifiers into the root script package with
`Options.Predeclared`. Each binding carries a runtime value and a `go/types`
type. `golark.Bind(value)` infers common types using reflection; callers may
supply `Binding.Type` explicitly for application-specific named types or types
that reflection cannot describe precisely.

The checker inserts these objects into the root package scope before checking.
Consequently, an invalid call such as `write(42)` is rejected before
evaluation when the host registered `write func(string) error`.

## 6. Runtime limits and security

Every interpreted operation consumes a step. `Options.MaxSteps` defaults to
five million steps; callers can select another budget. Entry points accept a
`context.Context`, which is checked during package resolution and evaluation
for cooperative cancellation. This bounds evaluator work, not arbitrary work
performed by a native host function or resolver. Host callbacks should honor
their own contexts and resource limits.

The interpreter is in-process, not an operating-system sandbox. A native
binding can intentionally or accidentally perform filesystem, network, process,
or other privileged operations. Do not expose those capabilities to untrusted
programs unless the host has independently constrained them. For hostile code,
use process isolation in addition to Golark's import policy and execution limits.

## 7. Completion plan

The current evaluator is a deliberately small execution slice, not a claim
that the complete Go specification is implemented. The checker can accept
constructs that the evaluator currently rejects. The next milestones should
close that gap in language layers:

1. Complete value/type construction, named types, method sets, interfaces,
   type assertions, and generic instantiation.
2. Complete statement semantics: labelled branches, `defer`, `go`, `select`,
   switch fallthrough, and panic/recover behavior.
3. Add closures with exact Go capture semantics, function values, method
   expressions/values, and all multi-result assignment contexts.
4. Expand package/runtime tests for initialization, package identity, export
   visibility, and source/native import interoperability.
5. Add conformance tests driven by the Go specification and differential tests
   against `go/types` and compiled Go snippets wherever observable behavior
   is comparable.

An implementation milestone is complete only when parser acceptance, type
checking, and execution support are covered by tests for the corresponding
language construct.
