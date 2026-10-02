package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	mdreplay "github.com/sanjeev2040/jev-trader/internal/marketdata/replay"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

// defaultDecisionLog is where run and replay keep the decision log.
const defaultDecisionLog = "state/decisions.db"

func runReplay(ctx context.Context, args []string, e env) int {
	if err := replayCmd(ctx, args, e); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader replay: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

func replayCmd(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dataDir := fs.String("data-dir", "data", "root directory of the Parquet store")
	source := fs.String("source", "ticks", "stored data to replay: ticks, bars (built from ticks) or backfill (Kite historical candles)")
	from := fs.String("from", "", "first IST day to replay, YYYY-MM-DD (default: from the start of the data)")
	to := fs.String("to", "", "last IST day to replay, YYYY-MM-DD (default: to the end of the data)")
	tokens := fs.String("tokens", "", "comma-separated Kite instrument tokens to replay (default: all, the index included)")
	index := fs.Uint("index", 0, "Kite instrument token of the NIFTY 50 index, used as snapshot context and never traded")
	deciders := fs.String("deciders", "hold,momentum", "comma-separated deciders: hold, momentum, jev, recorded:<run-id>")
	logPath := fs.String("log", defaultDecisionLog, "decision log (SQLite) the replay runs are written to")
	prefix := fs.String("run-prefix", "", "run id prefix; runs are <prefix>-<decider> (default: replay-<UTC time>)")
	latency := fs.Duration("latency", 0, "decision latency applied to every answer; 0 uses each decision's own (measured or recorded) latency")
	decideTimeout := fs.Duration("decide-timeout", 5*time.Second, "per-call decider deadline")
	model := fs.String("model", "", "pinned Jev model id (required for the jev decider)")
	jevURL := fs.String("jev-url", "", "Jev base URL (default: the public API)")
	horizon := fs.Int("horizon", replay.DefaultHorizon, "bars ahead BUY and SELL calls are scored for calibration")
	format := fs.String("format", "text", "report format: text or json")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader replay [flags]\n\nReplays stored market data through the trading engine with a simulated clock and\nthe paper broker, once per decider, and prints an evaluation report.\nNo Kite account is needed; the jev decider reads JEV_API_KEY.\n\nflags:")
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
	if *format != "text" && *format != "json" {
		return usageError{fmt.Errorf("--format %q is not text or json", *format)}
	}
	cfg, err := parseSource(*dataDir, *source, *from, *to, *tokens)
	if err != nil {
		return usageError{err}
	}
	if *index > uint(^uint32(0)) {
		return usageError{fmt.Errorf("--index %d is not a Kite instrument token", *index)}
	}
	names := splitList(*deciders)
	if len(names) == 0 {
		return usageError{errors.New("--deciders is empty")}
	}
	if *prefix == "" {
		*prefix = "replay-" + time.Now().UTC().Format("20060102T150405")
	}
	secrets := config.LoadSecrets(e.lookup)

	if err := os.MkdirAll(filepath.Dir(*logPath), 0o755); err != nil {
		return err
	}
	// A replay's log can be rebuilt, so it skips the per-write fsync.
	log, err := decisionlog.Open(*logPath, decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		return err
	}
	defer log.Close()

	specs, err := buildSpecs(ctx, names, log, secrets, *model, *jevURL)
	if err != nil {
		return usageError{err}
	}
	limits := risk.DefaultLimits()
	rep, err := replay.Run(ctx, replay.Config{
		Log: log,
		NewSource: func() (marketdata.Source, error) {
			return mdreplay.New(cfg)
		},
		SourceName: string(cfg.Kind), Label: cfg.Label(),
		Limits: limits, Paper: paper.DefaultConfig(), Bars: bars.Config{},
		Snapshot:   snapshot.Config{SizeThresholds: [2]float64{limits.SizeNotional[domain.SizeS], limits.SizeNotional[domain.SizeM]}},
		IndexToken: uint32(*index), RunPrefix: *prefix,
		Latency: *latency, DecideTimeout: *decideTimeout, Horizon: *horizon,
	}, specs)
	if err != nil {
		return err
	}

	w := e.stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if *format == "json" {
		return rep.WriteJSON(w)
	}
	return rep.WriteText(w)
}

// parseSource builds the replay source config from the flags. Days are IST
// calendar days; --to is inclusive.
func parseSource(dataDir, source, from, to, tokens string) (mdreplay.Config, error) {
	cfg := mdreplay.Config{Root: dataDir}
	switch source {
	case "ticks":
		cfg.Kind = mdreplay.Ticks
	case "bars":
		cfg.Kind = mdreplay.Bars
	case "backfill":
		cfg.Kind = mdreplay.Backfill
	default:
		return cfg, fmt.Errorf("--source %q is not ticks, bars or backfill", source)
	}
	var err error
	if from != "" {
		if cfg.From, err = time.ParseInLocation(time.DateOnly, from, ist); err != nil {
			return cfg, fmt.Errorf("--from: %w", err)
		}
	}
	if to != "" {
		last, err := time.ParseInLocation(time.DateOnly, to, ist)
		if err != nil {
			return cfg, fmt.Errorf("--to: %w", err)
		}
		cfg.To = last.AddDate(0, 0, 1)
	}
	for _, s := range splitList(tokens) {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 {
			return cfg, fmt.Errorf("invalid instrument token %q", s)
		}
		cfg.Tokens = append(cfg.Tokens, uint32(n))
	}
	if _, err := mdreplay.New(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// buildSpecs turns decider names into replay specs.
func buildSpecs(ctx context.Context, names []string, log *decisionlog.Log, secrets config.Secrets, model, jevURL string) ([]replay.Spec, error) {
	var specs []replay.Spec
	seen := map[string]bool{}
	for _, name := range names {
		kind, arg, _ := strings.Cut(name, ":")
		label := kind
		if arg != "" {
			label = kind + "-" + arg
		}
		if seen[label] {
			return nil, fmt.Errorf("decider %q is listed twice", name)
		}
		seen[label] = true
		switch kind {
		case "hold":
			specs = append(specs, replay.Spec{Name: label, Decider: decide.AlwaysHold{}})
		case "momentum":
			m, err := decide.NewMomentum(decide.MomentumConfig{})
			if err != nil {
				return nil, err
			}
			specs = append(specs, replay.Spec{Name: label, Decider: m})
		case "jev":
			d, err := newJev(secrets, model, jevURL)
			if err != nil {
				return nil, err
			}
			specs = append(specs, replay.Spec{Name: label, Decider: d})
		case "recorded":
			if arg == "" {
				return nil, errors.New("recorded needs a run id: recorded:<run-id>")
			}
			rd, err := log.RecordedDecider(ctx, arg)
			if err != nil {
				return nil, err
			}
			specs = append(specs, replay.Spec{Name: label, Decider: rd, Notes: func() []string {
				var notes []string
				if n := rd.Misses(); n > 0 {
					notes = append(notes, fmt.Sprintf("recorded replay of %s: %d snapshot(s) had no recorded answer and became HOLD; the data or the snapshot code differs from the original run.", arg, n))
				}
				if n := rd.Remaining(); n > 0 {
					notes = append(notes, fmt.Sprintf("recorded replay of %s: %d recorded answer(s) were never asked for.", arg, n))
				}
				return notes
			}})
		default:
			return nil, fmt.Errorf("unknown decider %q (want hold, momentum, jev or recorded:<run-id>)", name)
		}
	}
	return specs, nil
}

func newJev(secrets config.Secrets, model, baseURL string) (*jev.Client, error) {
	if err := secrets.RequireJev(); err != nil {
		return nil, err
	}
	if model == "" {
		return nil, errors.New("--model is required for the jev decider (pin an explicit model id)")
	}
	return jev.New(jev.Config{APIKey: secrets.JevAPIKey.Reveal(), Model: model, BaseURL: baseURL})
}
