// Command jevtrader is the jev-trader CLI. See reports/0002-architecture.md
// for its subcommands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string, env env) int
}

// env is the process environment a subcommand runs in, injectable for tests.
type env struct {
	lookup func(string) (string, bool)
	stdout io.Writer
	stderr io.Writer
}

var commands = map[string]command{
	"backfill": {summary: "fetch historical 1m candles from Kite and store them as bars", run: runBackfill},
	"login":    {summary: "daily Kite Connect login; saves the access token", run: runLogin},
	"record":   {summary: "ingest Kite ticks and persist them to Parquet (no trading)", run: runRecord},
	"replay":   {summary: "replay stored data through the trading engine and report against baselines", run: runReplay},
	"run":      {summary: "trade in paper mode on live Kite data (--mode=live is gated off)", run: runTrade},
	"study":    {summary: "backtest the pre-market picker over many days against buy-and-hold and a random list", run: runStudy},
	"screen":   {summary: "pick the day's mid and small-cap watchlist from news, sector strength and the pre-open", run: runScreen},
	"version":  {summary: "print the build version (the git SHA stamped into the image)", run: runVersion},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], env{lookup: os.LookupEnv, stdout: os.Stdout, stderr: os.Stderr})
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(e.stderr)
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(e.stderr, "jevtrader: unknown command %q\n", args[0])
		usage(e.stderr)
		return 2
	}
	return cmd.run(ctx, args[1:], e)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: jevtrader <command> [flags]\n\ncommands:")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-10s %s\n", name, commands[name].summary)
	}
}

func runRecord(_ context.Context, args []string, e env) int {
	return record(args, e.stderr)
}
