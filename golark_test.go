package golark_test

import (
	"context"
	"errors"
	"strings"
	stdstrings "strings"
	"testing"

	"github.com/ep0ll/golark-go"
)

func TestExecAndCall(t *testing.T) {
	source := []byte(`package config
const base = 40
const answer = base + 2
func Add(a, b int) int { return a + b }
func Sum(n int) int {
    total := 0
    for i := 0; i < n; i++ { total += i }
    return total
}
func Greeting(name string) string { return "hello, " + name }
`)
	module, err := golark.Exec(context.Background(), "config.go", source, golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Add", 20, 22)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("Add() = %#v, want 42", got)
	}
	got, err = module.Call(context.Background(), "Sum", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 45 {
		t.Fatalf("Sum() = %#v, want 45", got)
	}
	got, err = module.Call(context.Background(), "Greeting", "world")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "hello, world" {
		t.Fatalf("Greeting() = %#v", got)
	}
	if module.Globals()["answer"] != int(42) {
		t.Fatalf("global answer = %#v", module.Globals()["answer"])
	}
}

func TestTypedHostBinding(t *testing.T) {
	module, err := golark.Exec(context.Background(), "host.go", []byte(`package config
func BuildName(s string) string { return prefix(s) }
func Answer() int { return add(20, 22) }
`), golark.Options{Predeclared: map[string]golark.Binding{
		"prefix": golark.Bind(func(s string) string { return "build:" + s }),
		"add":    golark.Bind(func(a, b int) int { return a + b }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "BuildName", "image")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "build:image" {
		t.Fatalf("BuildName() = %#v", got)
	}
	got, err = module.Call(context.Background(), "Answer")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 42 {
		t.Fatalf("Answer() = %#v", got)
	}
}

func TestTypeErrorsHappenBeforeExecution(t *testing.T) {
	_, err := golark.Exec(context.Background(), "bad.go", []byte(`package config
func Run() { write(42) }
`), golark.Options{Predeclared: map[string]golark.Binding{"write": golark.Bind(func(string) {})}})
	if err == nil || !strings.Contains(err.Error(), "cannot use 42") {
		t.Fatalf("expected static type error, got %v", err)
	}
}

func TestRangeAndMapLiteral(t *testing.T) {
	module, err := golark.Exec(context.Background(), "range.go", []byte(`package config
func Sum(values []int) int {
    total := 0
    for _, value := range values { total += value }
    return total
}
func Make() map[string]int { return map[string]int{"a": 1, "b": 2} }
`), golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Sum", []int{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 10 {
		t.Fatalf("Sum() = %#v", got)
	}
	got, err = module.Call(context.Background(), "Make")
	if err != nil {
		t.Fatal(err)
	}
	value := got[0].(map[string]int)
	if value["a"] != 1 || value["b"] != 2 {
		t.Fatalf("Make() = %#v", value)
	}
}

func TestStepBudget(t *testing.T) {
	module, err := golark.Exec(context.Background(), "loop.go", []byte(`package config
func Run() { for { } }
`), golark.Options{MaxSteps: 20})
	if err != nil {
		t.Fatal(err)
	}
	_, err = module.Call(context.Background(), "Run")
	if err == nil || !strings.Contains(err.Error(), "step limit") {
		t.Fatalf("expected step limit error, got %v", err)
	}
}

func TestCancellation(t *testing.T) {
	module, err := golark.Exec(context.Background(), "cancel.go", []byte(`package config
func Run() { for { } }
`), golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = module.Call(ctx, "Run")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestParseFiles(t *testing.T) {
	program, err := golark.ParseFiles(map[string][]byte{
		"a.go": []byte("package config\nfunc A() int { return B() }\n"),
		"b.go": []byte("package config\nfunc B() int { return 42 }\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	module, err := golark.Execute(context.Background(), program, golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 42 {
		t.Fatalf("A() = %#v", got)
	}
}

func TestScriptFunctionPassedToHostCallback(t *testing.T) {
	module, err := golark.Exec(context.Background(), "callback.go", []byte(`package config
func Double(n int) int { return n * 2 }
func Run() int { return apply(Double, 21) }
`), golark.Options{Predeclared: map[string]golark.Binding{
		"apply": golark.Bind(func(fn func(int) int, n int) int { return fn(n) }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Run")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 42 {
		t.Fatalf("Run() = %#v, want 42", got)
	}
}

func TestVariadicScriptFunction(t *testing.T) {
	module, err := golark.Exec(context.Background(), "variadic.go", []byte(`package config
func Total(values ...int) int {
    total := 0
    for _, value := range values { total += value }
    return total
}
func Empty() int { return Total() }
`), golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Total", 1, 2, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 10 {
		t.Fatalf("Total() = %#v", got)
	}
	got, err = module.Call(context.Background(), "Empty")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0 {
		t.Fatalf("Empty() = %#v", got)
	}
}

func TestRuntimeImportFacade(t *testing.T) {
	module, err := golark.Exec(context.Background(), "imports.go", []byte(`package config
import "strings"
func Normalize(value string) string { return strings.TrimSpace(value) }
`), golark.Options{Imports: map[string]map[string]golark.Binding{
		"strings": {"TrimSpace": golark.Bind(stdstrings.TrimSpace)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Normalize", "  golark  ")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "golark" {
		t.Fatalf("Normalize() = %#v", got)
	}
}

type testCode int64

func (c testCode) Number() int { return int(c) }

func TestNamedHostTypeAndMethods(t *testing.T) {
	module, err := golark.Exec(context.Background(), "named.go", []byte(`package config
func Number() int { return code.Number() }
`), golark.Options{Predeclared: map[string]golark.Binding{
		"code": golark.Bind(testCode(42)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Number")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 42 {
		t.Fatalf("Number() = %#v", got)
	}
}

func TestRangeStringUsesByteOffsets(t *testing.T) {
	module, err := golark.Exec(context.Background(), "string-range.go", []byte(`package config
func Offsets(value string) []int { result := []int{}; for index, _ := range value { result = append(result, index) }; return result }
func Runes(value string) []rune { result := []rune{}; for _, r := range value { result = append(result, r) }; return result }
`), golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Offsets", "a界b")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 4}
	actual := got[0].([]int)
	if len(actual) != len(want) {
		t.Fatalf("Offsets() = %#v, want %#v", actual, want)
	}
	for i := range want {
		if actual[i] != want[i] {
			t.Fatalf("Offsets() = %#v, want %#v", actual, want)
		}
	}
	got, err = module.Call(context.Background(), "Runes", "a界b")
	if err != nil {
		t.Fatal(err)
	}
	actualRunes := got[0].([]rune)
	if string(actualRunes) != "a界b" {
		t.Fatalf("Runes() = %#v", actualRunes)
	}
}


func TestIfInitHasItsOwnScope(t *testing.T) {
	module, err := golark.Exec(context.Background(), "if-scope.go", []byte(`package config
func Scope() int {
    value := 1
    if value := 2; value == 2 { value = 3 }
    return value
}
`), golark.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := module.Call(context.Background(), "Scope")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 1 {
		t.Fatalf("Scope() = %#v, want outer value 1", got)
	}
}
