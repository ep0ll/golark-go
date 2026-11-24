# Golark architecture

## 1. The design decision

Starlark is an interpreter with its own scanner, parser, resolver, compiler,
bytecode format, value model, and evaluator. Golark takes a different route:
its source language is Go, so the Go toolchain's front end is the language
front end. `go/parser` builds the AST; `go/types` resolves names and validates
static types; `go/constant` preserves compile-time constant semantics; and
`go/format` provides canonical formatting.

The standard library does **not** provide an evaluator for Go ASTs. The
`internal/eval` package is therefore a separate runtime implementation. This
separation is fundamental: parser acceptance and successful type checking do
not imply that every AST node is already executable by Golark.

## 2. Package boundaries

* `internal/syntax`: thin, policy-free wrappers around `go/parser` and
  `go/format`. No custom tokens or grammar.
* `internal/checker`: builds a package scope, inserts typed host bindings, and
  invokes `go/types`. It owns the checked-program representation.
* `internal/eval`: runtime environments, module initialization, function
  invocation, reflection-based host calls, expression evaluation, statement
  execution, cancellation, and step accounting.
* `golark` (module root): stable embedding API. It translates public options
  into internal package options without leaking evaluator implementation types.
* `cmd/golark`: command-line entry point (being expanded alongside the runtime).

The dependency direction is deliberately one-way:

```text
public API -> syntax/checker/eval -> Go standard library
```

The checker does not depend on the evaluator, and syntax does not depend on
runtime values. This allows parsing, formatting, static analysis, and execution
to be used independently in future APIs.

## 3. Static host bindings

An application may seed identifiers into a script package with
`Options.Predeclared`. Each binding carries a runtime value and a `go/types`
type. `golark.Bind(value)` infers common types using reflection; callers may
supply `Binding.Type` explicitly for application-specific named types or types
that reflection cannot describe precisely.

The checker inserts these objects into the script package scope before calling
`types.Checker.Files`. Consequently, an invalid call such as `write(42)` is
rejected before evaluation when the host registered `write func(string) error`.

Imports are split into two independent concerns:

1. `types.Importer` provides the package's static exported declarations.
2. `Options.Imports` provides the runtime values for those declarations.

This explicit split prevents runtime reflection from becoming the source of
truth for static types and makes it possible to supply carefully restricted
host facades.

## 4. Module and function model

A module owns a package-level environment. Functions are registered before
package variable initializers run, allowing forward references and recursion.
Go's `types.Info.InitOrder` provides dependency-aware ordering for initialized
package variables. `init` functions run after package initialization.

A script function closes over its defining environment. Each invocation creates
a fresh local environment whose parent is the module environment. Host
functions are called through reflection after argument validation/conversion.
The public `Module.Call` method provides dynamic invocation from the host while
the function body remains statically type-checked.

## 5. Runtime limits

Every interpreted operation consumes a step. `Options.MaxSteps` defaults to
five million steps; callers can select another budget. All entry points accept
a `context.Context`, which is checked during evaluation for cooperative
cancellation. This bounds evaluator work, not arbitrary work performed by a
host function. Host callbacks should honor their own contexts and resource
limits. Step accounting does not provide a memory limit or sandbox; untrusted
code should execute in a separately constrained process.

## 6. Semantic boundary and completion plan

The current evaluator is a deliberately small first execution slice, not a
claim that the complete Go specification is implemented. The checker can
accept constructs that the evaluator currently rejects. The next milestones
should close that gap in language layers:

1. Complete value/type construction, named types, method sets, interfaces,
   type assertions, and generic instantiation.
2. Complete statement semantics: labelled branches, `defer`, `go`, `select`,
   switch fallthrough, and panic/recover behavior.
3. Add closures with exact Go capture semantics, variadic calls, function
   values, method expressions/values, and multi-result assignment contexts.
4. Add package/module loading with an explicit loader/cache interface and
   cycle detection, while preserving package initialization rules.
5. Add conformance tests driven by the Go specification and differential tests
   against `go/types` and compiled Go snippets wherever observable behavior
   is comparable.
6. Decide the intended security profile. A static Go language is powerful and
   is not automatically safe for untrusted configuration; capabilities should
   be controlled by imports and process isolation.

An implementation milestone is complete only when parser acceptance, type
checking, and execution support are covered by tests for the corresponding
language construct.
