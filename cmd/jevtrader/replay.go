package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	mdreplay "github.com/sanjeev2040/jev-trader/internal/marketdata/replay"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
	"github.com/sanjeev2040/jev-trader/internal/store"
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
	tokens := fs.String("tokens", "", "comma-separated Kite instrument tokens to replay (default: all)")
	indexKey := fs.String("index", "NSE:NIFTY 50", `index used as snapshot context and never traded, found by EXCHANGE:SYMBOL in the stored data, or given as an instrument token (NIFTY 50 is 256265); "" for none`)
	start := fs.String("start", "", "first IST time of day to trade, HH:MM; earlier bars of the day still warm the indicators up but are not decided on (default: the session open)")
	end := fs.String("end", "", "IST time of day to stop at, HH:MM; later data is not traded, only used for the comparison (default: the close of the data)")
	walkthrough := fs.String("walkthrough", "", `add each run's session to the report, instrument by instrument: "actions" (BUY/SELL answers, risk exits, fills) or "all" (every decision, HOLDs too)`)
	compare := fs.Bool("compare", false, "add the comparison with the real market: the price after each BUY/SELL, and PnL against buy-and-hold and the hold and momentum baselines (those two are replayed too)")
	lookahead := fs.String("lookahead", "5m,15m,30m", "with --compare, how long after each BUY/SELL to check the price, comma-separated")
	deciders := fs.String("deciders", "hold,momentum", "comma-separated deciders: hold, momentum, jev, recorded:<run-id>")
	logPath := fs.String("log", defaultDecisionLog, "decision log (SQLite) the replay runs are written to")
	prefix := fs.String("run-prefix", "", "run id prefix; runs are <prefix>-<decider> (default: replay-<UTC time>)")
	latency := fs.Duration("latency", 0, "decision latency applied to every answer; 0 uses each decision's own (measured or recorded) latency")
	decideTimeout := fs.Duration("decide-timeout", 5*time.Second, "per-call decider deadline")
	model := fs.String("model", jev.DefaultModel, "pinned Jev model id for the jev decider (\"jev-latest\" is rejected)")
	jevURL := fs.String("jev-url", "", "Jev base URL (default: the public API)")
	horizon := fs.Int("horizon", replay.DefaultHorizon, "bars ahead BUY and SELL calls are scored for calibration")
	format := fs.String("format", "text", "report format: text or json")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader replay [flags]\n\nReplays stored market data through the trading engine with a simulated clock and\nthe paper broker, once per decider, and prints an evaluation report.\nNo Kite account is needed; the jev decider reads JEV_API_KEY, or jev-api-key from the\nAzure Key Vault named by JEVTRADER_KEYVAULT.\n\nflags:")
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
	indexToken, err := resolveIndex(&cfg, *indexKey)
	if err != nil {
		return err
	}
	if *indexKey != "" && indexToken == 0 {
		fmt.Fprintf(e.stderr, "jevtrader replay: index %q is not in the stored data, so snapshots have no index context (backfill with --symbols to store symbols, or pass --index <token>)\n", *indexKey)
	}
	names := splitList(*deciders)
	if len(names) == 0 {
		return usageError{errors.New("--deciders is empty")}
	}
	window, err := parseWindow(*start, *end)
	if err != nil {
		return usageError{err}
	}
	var walk replay.WalkMode
	switch replay.WalkMode(*walkthrough) {
	case replay.WalkOff, replay.WalkActions, replay.WalkAll:
		walk = replay.WalkMode(*walkthrough)
	default:
		return usageError{fmt.Errorf("--walkthrough %q is not actions or all", *walkthrough)}
	}
	horizons, err := parseLookahead(*lookahead)
	if err != nil {
		return usageError{err}
	}
	if *compare {
		// The comparison needs the baselines to compare with.
		for _, baseline := range []string{"hold", "momentum"} {
			if !slices.Contains(names, baseline) {
				names = append(names, baseline)
				fmt.Fprintf(e.stderr, "jevtrader replay: --compare also replays the %s baseline\n", baseline)
			}
		}
	}
	if *prefix == "" {
		*prefix = "replay-" + time.Now().UTC().Format("20060102T150405")
	}
	var wanted []string
	for _, name := range names {
		if kind, _, _ := strings.Cut(name, ":"); kind == "jev" {
			wanted = []string{config.EnvJevAPIKey}
		}
	}
	secrets, err := loadSecrets(ctx, e, wanted...)
	if err != nil {
		return usageError{err}
	}

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
		IndexToken: indexToken, RunPrefix: *prefix, Window: window,
		Latency: *latency, DecideTimeout: *decideTimeout, Horizon: *horizon,
		Walkthrough: walk, Compare: *compare, Lookahead: horizons,
		BuyHoldNotional: limits.SizeNotional[domain.SizeM],
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

// parseWindow turns --start and --end (IST, HH:MM) into an engine window.
func parseWindow(start, end string) (engine.Window, error) {
	var w engine.Window
	parse := func(flagName, v string) (time.Duration, error) {
		t, err := time.Parse("15:04", v)
		if err != nil {
			return 0, fmt.Errorf("--%s %q is not a time of day, HH:MM", flagName, v)
		}
		return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
	}
	var err error
	if start != "" {
		if w.Start, err = parse("start", start); err != nil {
			return w, err
		}
	}
	if end != "" {
		if w.End, err = parse("end", end); err != nil {
			return w, err
		}
		if w.End <= w.Start {
			return w, fmt.Errorf("--end %s is not after --start %s", end, start)
		}
	}
	return w, nil
}

// parseLookahead parses --lookahead, a list of positive whole-minute durations.
func parseLookahead(list string) ([]time.Duration, error) {
	var out []time.Duration
	for _, s := range splitList(list) {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Minute || d%time.Minute != 0 {
			return nil, fmt.Errorf("--lookahead %q: want whole minutes such as 5m", s)
		}
		out = append(out, d)
	}
	return out, nil
}

var errIndexFound = errors.New("index found")

// resolveIndex finds the index token in the stored data by its
// EXCHANGE:SYMBOL key, and makes sure a token filter still lets the index
// through as context. Zero means the store holds no such index. The engine
// never trades IndexToken.
func resolveIndex(cfg *mdreplay.Config, key string) (uint32, error) {
	if key == "" {
		return 0, nil
	}
	if n, err := strconv.ParseUint(key, 10, 32); err == nil && n > 0 {
		// Backfilled bars made from tokens alone carry no symbol to find the
		// index by, so it can be named by token.
		token := uint32(n)
		if len(cfg.Tokens) > 0 && !slices.Contains(cfg.Tokens, token) {
			cfg.Tokens = append(cfg.Tokens, token)
		}
		return token, nil
	}
	q := store.Query{From: cfg.From, To: cfg.To}
	var token uint32
	found := func(t uint32, symbol string) error {
		if symbol != key {
			return nil
		}
		token = t
		return errIndexFound
	}
	var err error
	if cfg.Kind == mdreplay.Ticks {
		err = store.ScanTicks(cfg.Root, q, func(t domain.Tick) error {
			return found(t.InstrumentToken, t.Symbol)
		})
	} else {
		interval := cfg.BarInterval
		if interval == 0 {
			interval = time.Minute
		}
		err = store.ScanBars(cfg.Root, string(cfg.Kind), store.BarQuery{Query: q, Interval: interval}, func(b domain.Bar) error {
			return found(b.Instrument.Token, b.Instrument.Exchange+":"+b.Instrument.Symbol)
		})
	}
	if err != nil && !errors.Is(err, errIndexFound) {
		return 0, fmt.Errorf("resolving index %q: %w", key, err)
	}
	if token == 0 {
		return 0, nil
	}
	if len(cfg.Tokens) > 0 && !slices.Contains(cfg.Tokens, token) {
		cfg.Tokens = append(cfg.Tokens, token)
	}
	return token, nil
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
	return jev.New(jev.Config{APIKey: secrets.JevAPIKey.Reveal(), Model: model, BaseURL: baseURL})
}
