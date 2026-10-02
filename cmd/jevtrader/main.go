// Command jevtrader is the jev-trader service binary. Each subcommand wires a
// different set of parts into the same engine (reports/0002-architecture.md).
package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string, env func(string) string, stderr io.Writer) error
}

var commands = map[string]command{
	"backfill": {"fetch historical 1m candles from Kite and store them as bars", runBackfill},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, env func(string) string, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "jevtrader: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err := cmd.run(ctx, args[1:], env, stderr); err != nil {
		fmt.Fprintf(stderr, "jevtrader %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: jevtrader <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	for _, name := range slices.Sorted(maps.Keys(commands)) {
		fmt.Fprintf(w, "  %-10s %s\n", name, commands[name].summary)
	}
}
