package main

import (
	"context"
	"fmt"

	"github.com/ep0ll/golark-go"
)

func main() {
	source := []byte(`package config

const base = 40
const answer = base + 2

func Sum(n int) int {
    total := 0
    for i := 0; i < n; i++ {
        total += i
    }
    return total
}

func Describe(name string) string {
    return "hello, " + name
}
`)
	module, err := golark.Exec(context.Background(), "config.go", source, golark.Options{})
	if err != nil {
		panic(err)
	}
	sum, err := module.Call(context.Background(), "Sum", 10)
	if err != nil {
		panic(err)
	}
	greeting, err := module.Call(context.Background(), "Describe", "golark")
	if err != nil {
		panic(err)
	}
	fmt.Println("sum:", sum[0])
	fmt.Println("greeting:", greeting[0])
	fmt.Println("globals:", module.Globals())
}
