package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/shadow"
)

func runShadowReport(ctx context.Context, args []string, e env) int {
	if err := shadowReport(ctx, args, e, time.Now); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader shadow-report: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

func shadowReport(ctx context.Context, args []string, e env, now func() time.Time) error {
	fs := flag.NewFlagSet("shadow-report", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	from := fs.String("from", "", "first IST day, YYYY-MM-DD (required)")
	to := fs.String("to", "", "last IST day, YYYY-MM-DD (default: --from)")
	logPath := fs.String("log", defaultDecisionLog, "decision log (SQLite) holding the shadow signals")
	dataDir := fs.String("data-dir", "data", "directory of the recorded (and backfilled) bars the outcomes are read from")
	benchmark := fs.String("benchmark", shadow.BenchmarkIndex, "what a return is netted against: index (the index the run watched), universe (the mean of the day's screened stocks, as recorded) or none")
	fill := fs.Bool("fill", true, "first write the outcomes the recorded bars now answer (the report reads only what is in the log)")
	byVerdict := fs.Bool("by-verdict", false, "also split each decision signal by the risk gate's verdict")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	resamples := fs.Int("resamples", shadow.DefaultResamples, "day resamples of the confidence interval")
	seed := fs.Uint64("seed", 1, "seed of the resampling, so a report can be reproduced")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader shadow-report --from YYYY-MM-DD [--to YYYY-MM-DD] [flags]\n\nSummarises the shadow log of `jevtrader run`: for every signal (the picker's entries and each decider\nanswer, traded or not), the count, the mean market-neutral return at +15m, +60m, the window end and\nthe day close with a 95% interval from resampling days, and the hit rate.\n\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected arguments %q", fs.Args())}
	}
	if *to == "" {
		*to = *from
	}
	for name, v := range map[string]string{"--from": *from, "--to": *to} {
		if _, err := time.ParseInLocation(time.DateOnly, v, ist); err != nil {
			return usageError{fmt.Errorf("%s: want a YYYY-MM-DD day, got %q", name, v)}
		}
	}
	if *to < *from {
		return usageError{fmt.Errorf("--to %s is before --from %s", *to, *from)}
	}
	if *resamples < 1 {
		return usageError{errors.New("--resamples must be positive")}
	}
	// Open would create an empty log where the path is wrong; say so instead.
	if _, err := os.Stat(*logPath); err != nil {
		return fmt.Errorf("decision log: %w", err)
	}
	log, err := decisionlog.Open(*logPath, decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		return err
	}
	defer log.Close()

	if *fill {
		res, err := shadow.Fill(ctx, log, *dataDir, *from, *to, now())
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stderr, "jevtrader shadow-report: %d signals, %d outcomes written, %d not answered yet by the bars in %s\n", res.Signals, res.Filled, res.Pending, *dataDir)
	}
	signals, err := log.ShadowSignals(ctx, *from, *to)
	if err != nil {
		return err
	}
	outcomes, err := log.ShadowOutcomes(ctx, *from, *to)
	if err != nil {
		return err
	}
	rep, err := shadow.BuildReport(signals, outcomes, shadow.Options{
		From: *from, To: *to, Benchmark: *benchmark, ByVerdict: *byVerdict, Resamples: *resamples, Seed: *seed,
	})
	if err != nil {
		return usageError{err}
	}
	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return rep.Write(e.stdout)
}
