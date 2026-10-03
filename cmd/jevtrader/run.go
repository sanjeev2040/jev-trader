package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/clock"
	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/kiteauth"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/marketdata/kite"
	kitebroker "github.com/sanjeev2040/jev-trader/internal/orders/kite"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
	"github.com/sanjeev2040/jev-trader/internal/store"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

const modePaper = "paper"

func runTrade(ctx context.Context, args []string, e env) int {
	if err := trade(ctx, args, e); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader run: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

func trade(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	mode := fs.String("mode", modePaper, "paper (simulated fills on live data) or live (gated off; refuses to start)")
	liveEnabled := fs.Bool("live-enabled", false, "live.enabled: one of the two switches live trading needs besides --mode=live (it is also gated off in code)")
	watchlist := fs.String("watchlist", "", `comma-separated instruments, e.g. "NSE:INFY,NSE:TCS" (exact EXCHANGE:SYMBOL keys; required unless --watchlist-file is given, and the fallback when that file is missing or stale)`)
	watchlistFile := fs.String("watchlist-file", "", "today's watchlist file written by `jevtrader screen` (e.g. state/watchlist/2026-10-05.json); used instead of --watchlist when it exists and is for today's IST date")
	capitalFlag := fs.Float64("capital", 0, capitalFlagHelp)
	indexKey := fs.String("index", "NSE:NIFTY 50", `index used as snapshot context and never traded; "" for none`)
	model := fs.String("model", jev.DefaultModel, "pinned Jev model id (\"jev-latest\" is rejected)")
	jevURL := fs.String("jev-url", "", "Jev base URL (default: the public API)")
	decideTimeout := fs.Duration("decide-timeout", engine.DefaultDecideTimeout, "per-call Jev deadline")
	dataDir := fs.String("data-dir", "data", "directory for ticks, bars and the cached instrument master")
	stateDir := fs.String("state-dir", "", "state directory holding the Kite session and the KILL file (default $XDG_STATE_HOME/jevtrader or ~/.local/state/jevtrader)")
	logPath := fs.String("log", defaultDecisionLog, "decision log (SQLite)")
	record := fs.Bool("record", true, "also record ticks and closed bars under --data-dir")
	buffer := fs.Int("buffer", 8192, "market-data channel capacity")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader run (--watchlist NSE:INFY,NSE:TCS | --watchlist-file state/watchlist/YYYY-MM-DD.json) [flags]\n\nTrades on live Kite data with Jev decisions, the risk gate and the paper broker.\nRun `jevtrader login` first. Reads KITE_API_KEY, KITE_API_SECRET and JEV_API_KEY from the environment, or from the\nAzure Key Vault named by JEVTRADER_KEYVAULT.\n\nflags:")
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
	if err := liveGate(*mode, *liveEnabled); err != nil {
		return err
	}
	static := splitList(*watchlist)
	switch {
	case len(static) == 0 && *watchlistFile == "":
		return usageError{errors.New("--watchlist or --watchlist-file is required")}
	case *buffer < 1:
		return usageError{errors.New("--buffer must be positive")}
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, nil))
	symbols, err := resolveWatchlist(static, *watchlistFile, time.Now(), logger)
	if err != nil {
		return err
	}

	secrets, err := loadSecrets(ctx, e, config.EnvKiteAPIKey, config.EnvKiteAPISecret, config.EnvJevAPIKey)
	if err != nil {
		return usageError{err}
	}
	if err := secrets.RequireKite(); err != nil {
		return usageError{err}
	}
	capital, err := resolveCapital(ctx, e, *capitalFlag)
	if err != nil {
		return err
	}
	limits := tradingLimits(capital)
	logCapital(logger, capital, limits)
	jevClient, err := newJev(secrets, *model, *jevURL)
	if err != nil {
		return usageError{err}
	}
	if *stateDir == "" {
		if *stateDir, err = kiteauth.DefaultStateDir(); err != nil {
			return err
		}
	}

	// The token must verify before the run trades.
	auth := &kiteauth.Auth{Store: kiteauth.Store{Dir: *stateDir}}
	if _, err := auth.Verify(ctx, newKiteClient(secrets)); err != nil {
		return err
	}
	tokens := authToken{auth}
	apiKey := secrets.KiteAPIKey.Reveal()

	all := symbols
	if *indexKey != "" {
		all = append(append([]string(nil), symbols...), *indexKey)
	}
	exchanges, err := kite.Exchanges(all)
	if err != nil {
		return usageError{err}
	}
	master, err := kite.LoadMaster(ctx, *dataDir, time.Now(), exchanges, kite.HTTPMasterFetcher(nil, "", apiKey, tokens))
	if err != nil {
		return err
	}
	instruments, err := kite.Resolve(master, all)
	if err != nil {
		return usageError{err}
	}
	var indexToken uint32
	if *indexKey != "" {
		indexToken = instruments[len(instruments)-1].Token
	}
	src, err := kite.New(kite.Config{APIKey: apiKey, Tokens: tokens, Instruments: instruments, Logger: logger})
	if err != nil {
		return err
	}

	return tradeSession(ctx, tradeConfig{
		runID: modePaper + "-" + time.Now().UTC().Format("20060102T150405"), mode: *mode,
		source: src, sourceName: "kite", decider: jevClient, deciderName: "jev",
		logPath: *logPath, dataDir: *dataDir, stateDir: *stateDir,
		indexToken: indexToken, record: *record, buffer: *buffer, decideTimeout: *decideTimeout,
		limits: limits, clock: clock.Wall{}, logger: logger, instruments: len(symbols),
	})
}

// resolveWatchlist returns the instruments to trade: the watchlist file's, if
// there is one for today, otherwise the static list. A file that is missing,
// stale (another IST day) or unreadable is not an error while a static list
// exists: the session trades the static list and says why. With no static list
// to fall back on it is.
func resolveWatchlist(static []string, path string, now time.Time, logger *slog.Logger) ([]string, error) {
	if path == "" {
		return static, nil
	}
	f, err := watchlist.Load(path, now)
	if err == nil {
		logger.Info("watchlist from file", "path", path, "date", f.Date, "phase", f.Phase, "final", f.Final,
			"generated_at", f.GeneratedAt.Format(time.RFC3339), "instruments", strings.Join(f.Keys(), ","))
		return f.Keys(), nil
	}
	if len(static) == 0 {
		return nil, fmt.Errorf("watchlist file: %w (and no --watchlist to fall back on)", err)
	}
	logger.Warn("watchlist file not used; trading the static --watchlist", "path", path, "reason", err.Error(), "instruments", strings.Join(static, ","))
	return static, nil
}

// liveGate keeps run from ever placing a real order. Paper passes. Live asks
// the Kite broker's own gate, which refuses until live trading is approved in
// code, enabled in config and requested with --mode=live; and even if that
// gate opened, run has no live wiring, so it still refuses. Enabling live
// trading is a separately reviewed change.
func liveGate(mode string, liveEnabled bool) error {
	switch mode {
	case modePaper:
		return nil
	case kitebroker.ModeLive:
		if _, err := kitebroker.New(kitebroker.Config{LiveEnabled: liveEnabled, Mode: mode}, nil); err != nil {
			return err
		}
		return errors.New("live trading is not wired into run; it is enabled by a separate, reviewed change")
	}
	return usageError{fmt.Errorf("--mode %q is not paper or live", mode)}
}

// authToken adapts kiteauth to the ticker's token provider.
type authToken struct{ auth *kiteauth.Auth }

func (t authToken) Token(ctx context.Context) (string, error) {
	s, err := t.auth.Token(ctx)
	if err != nil {
		return "", err
	}
	return s.Reveal(), nil
}

// tradeConfig is everything a paper session needs; trade builds it from flags
// and Kite, tests build it from fixtures.
type tradeConfig struct {
	runID         string
	mode          string
	source        marketdata.Source
	sourceName    string
	decider       decide.Decider
	deciderName   string
	logPath       string
	dataDir       string
	stateDir      string // watched for the KILL file; empty disables the watcher
	indexToken    uint32
	record        bool
	buffer        int
	decideTimeout time.Duration
	limits        risk.Limits
	clock         clock.Clock
	logger        *slog.Logger
	instruments   int // traded instruments, for the log
}

// tradeSession runs the engine on the wall clock with the paper broker until
// ctx ends or the source fails, recording ticks and bars on the way.
func tradeSession(ctx context.Context, rc tradeConfig) (err error) {
	if err := os.MkdirAll(filepath.Dir(rc.logPath), 0o755); err != nil {
		return err
	}
	log, err := decisionlog.Open(rc.logPath, decisionlog.Options{}) // fsync each write: a crash must be reconcilable
	if err != nil {
		return err
	}
	defer log.Close()

	gate, err := risk.NewGate(rc.limits)
	if err != nil {
		return err
	}
	broker, err := paper.New(rc.clock, paper.DefaultConfig())
	if err != nil {
		return err
	}
	now := rc.clock.Now()
	dayStart := time.Date(now.In(risk.IST).Year(), now.In(risk.IST).Month(), now.In(risk.IST).Day(), 0, 0, 0, 0, risk.IST)
	recorded, err := log.IntentsSince(ctx, rc.mode, dayStart)
	if err != nil {
		return err
	}

	cfg := engine.Config{
		RunID: rc.runID, Mode: rc.mode, DeciderName: rc.deciderName, SourceName: rc.sourceName,
		Source: rc.source, Clock: rc.clock, Decider: rc.decider, Broker: broker, Gate: gate, Log: log,
		Bars:       bars.Config{},
		Snapshot:   snapshot.Config{SizeThresholds: sizeThresholds(rc.limits)},
		IndexToken: rc.indexToken, DecideTimeout: rc.decideTimeout, AdvanceEvery: time.Second,
		EventBuffer: rc.buffer, Recorded: recorded, Logger: rc.logger,
	}

	var recWG sync.WaitGroup
	if rc.record {
		stop, err := startRecording(ctx, rc, &cfg, &recWG)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, stop()) }()
	}
	if rc.stateDir != "" {
		kill := make(chan risk.Reason, 4)
		cfg.Kill = kill
		go func() {
			if err := risk.WatchKill(ctx, rc.stateDir, time.Second, kill); err != nil && !errors.Is(err, context.Canceled) {
				rc.logger.Error("kill watcher stopped", "err", err)
			}
		}()
	}

	eng, err := engine.New(cfg)
	if err != nil {
		return err
	}
	rc.logger.Info("trading", "run", rc.runID, "mode", rc.mode, "decider", rc.deciderName, "instruments", rc.instruments, "budget", rc.limits.CapitalBudget)
	res, err := eng.Run(ctx)
	rc.logger.Info("stopped", "cycles", res.Cycles, "orders", res.OrdersPlaced, "rotations", res.Rotations, "fills", res.Fills, "pnl", res.PnL, "open_positions", len(res.Positions))
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// startRecording tees ticks to the Parquet tick recorder on its own goroutine,
// so recording stays off the decision path, and writes closed bars to the bar
// store. The returned stop function drains and closes both. A tick that does
// not fit the recorder's buffer is dropped and counted; a drop is a defect.
func startRecording(ctx context.Context, rc tradeConfig, cfg *engine.Config, wg *sync.WaitGroup) (stop func() error, err error) {
	barWriter, err := store.NewBarWriter(rc.dataDir, store.KindBars)
	if err != nil {
		return nil, err
	}
	ticks := make(chan domain.Tick, rc.buffer)
	var dropped atomic.Uint64
	cfg.TickSink = func(t domain.Tick) {
		select {
		case ticks <- t:
		default:
			if n := dropped.Add(1); n == 1 || n%1000 == 0 {
				rc.logger.Error("tick recorder is full; tick dropped", "dropped", n)
			}
		}
	}
	var barErr error
	cfg.BarSink = func(b domain.Bar) {
		if barErr == nil {
			if barErr = barWriter.Write(b); barErr != nil {
				rc.logger.Error("bar store write failed; bars are no longer recorded", "err", barErr)
			}
		}
	}

	recCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	rotate := time.NewTicker(10 * time.Second)
	recErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		recErr <- store.RecordTicks(recCtx, ticks, store.NewTickWriter(rc.dataDir, store.DefaultRotation), rotate.C)
	}()
	return func() error {
		close(ticks) // the engine has returned; nothing sends any more
		wg.Wait()
		rotate.Stop()
		cancel()
		if n := dropped.Load(); n > 0 {
			rc.logger.Error("ticks dropped by the recorder", "dropped", n)
		}
		return errors.Join(<-recErr, barErr, barWriter.Close())
	}, nil
}
