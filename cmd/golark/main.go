// Command golark runs a Go-syntax Golark module and prints its exported globals.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/ep0ll/golark-go"
)

func main() {
	show := flag.Bool("globals", true, "print exported module globals after initialization")
	steps := flag.Int64("max-steps", 5_000_000, "maximum interpreted execution steps (0 selects default)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: golark [flags] <file.go>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	module, err := golark.ExecFile(context.Background(), flag.Arg(0), golark.Options{MaxSteps: *steps})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *show {
		globals := module.Globals()
		keys := make([]string, 0, len(globals))
		for k := range globals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Printf("%s = %#v\n", key, globals[key])
		}
	}
}
