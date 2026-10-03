package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/marketdata/kite"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

// envAccessToken is the stub token provider used until kiteauth (#3) lands:
// it reads the day's access token from KITE_ACCESS_TOKEN.
type envAccessToken struct{}

func (envAccessToken) Token(context.Context) (string, error) {
	if t := os.Getenv("KITE_ACCESS_TOKEN"); t != "" {
		return t, nil
	}
	return "", errors.New("no Kite access token: set KITE_ACCESS_TOKEN (jevtrader login is not available yet)")
}

// record runs ingest and the tick recorder alone, with no trading.
func record(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watchlist := fs.String("watchlist", "", `comma-separated instruments, e.g. "NSE:INFY,NSE:TCS,NSE:NIFTY 50" (exact EXCHANGE:SYMBOL keys)`)
	dataDir := fs.String("data-dir", "data", "directory for ticks and the cached instrument master")
	buffer := fs.Int("buffer", 8192, "tick channel capacity; ticks are dropped when it is full")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	symbols := splitList(*watchlist)
	if len(symbols) == 0 {
		fmt.Fprintln(stderr, "jevtrader record: -watchlist is required")
		return 2
	}
	apiKey := os.Getenv("KITE_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(stderr, "jevtrader record: KITE_API_KEY is not set")
		return 2
	}
	if *buffer < 1 {
		fmt.Fprintln(stderr, "jevtrader record: -buffer must be positive")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := recordTicks(ctx, log, recordConfig{
		apiKey:    apiKey,
		tokens:    envAccessToken{},
		watchlist: symbols,
		dataDir:   *dataDir,
		buffer:    *buffer,
	}); err != nil {
		log.Error("record failed", "err", err)
		return 1
	}
	return 0
}

type recordConfig struct {
	apiKey    string
	tokens    kite.TokenProvider
	watchlist []string
	dataDir   string
	buffer    int
}

func recordTicks(ctx context.Context, log *slog.Logger, cfg recordConfig) error {
	exchanges, err := kite.Exchanges(cfg.watchlist)
	if err != nil {
		return err
	}
	master, err := kite.LoadMaster(ctx, cfg.dataDir, time.Now(), exchanges,
		kite.HTTPMasterFetcher(nil, "", cfg.apiKey, cfg.tokens))
	if err != nil {
		return err
	}
	instruments, err := kite.Resolve(master, cfg.watchlist)
	if err != nil {
		return err
	}
	src, err := kite.New(kite.Config{
		APIKey:      cfg.apiKey,
		Tokens:      cfg.tokens,
		Instruments: instruments,
		Logger:      log,
	})
	if err != nil {
		return err
	}

	// If the recorder fails, stop ingest too.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	events := make(chan marketdata.Event, cfg.buffer)
	recCtx, recCancel := context.WithCancel(context.Background())
	defer recCancel()
	ticks := marketdata.Ticks(recCtx, events, cfg.buffer)
	rotate := time.NewTicker(10 * time.Second)
	defer rotate.Stop()
	recDone := make(chan error, 1)
	go func() {
		err := store.RecordTicks(recCtx, ticks, store.NewTickWriter(cfg.dataDir, store.DefaultRotation), rotate.C)
		if err != nil {
			cancel(err)
		}
		recCancel()
		recDone <- err
	}()

	stats := time.NewTicker(time.Minute)
	defer stats.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-stats.C:
				s := src.Stats()
				log.Info("ingest stats", "published", s.Published, "dropped", s.Dropped, "connects", s.Connects, "reconnects", s.Reconnects)
			}
		}
	}()

	log.Info("recording", "instruments", len(instruments), "data_dir", cfg.dataDir)
	runErr := src.Run(ctx, events)
	// Run sends nothing after it returns, so the recorder can drain and stop.
	close(events)
	recErr := <-recDone

	s := src.Stats()
	log.Info("stopped", "published", s.Published, "dropped", s.Dropped)
	return errors.Join(runErr, recErr)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
