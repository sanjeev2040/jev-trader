package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

// Kite limits: historical data is 3 requests per second, and one minute-candle
// request may span at most 60 days.
const (
	historicalRate = 3
	maxChunkDays   = 60
)

var ist = time.FixedZone("IST", 5*3600+30*60)

// historian fetches Kite historical candles; *kiteconnect.Client satisfies it.
type historian interface {
	GetHistoricalData(token int, interval string, from, to time.Time, continuous, oi bool) ([]kiteconnect.HistoricalData, error)
}

type backfillJob struct {
	tokens   []uint32
	from, to time.Time // first and last IST day, at midnight
	derived  []time.Duration
	dataDir  string
}

func runBackfill(ctx context.Context, args []string, env func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tokens := fs.String("tokens", "", "comma-separated Kite instrument tokens (required)")
	from := fs.String("from", "", "first IST day to fetch, YYYY-MM-DD (required)")
	to := fs.String("to", "", "last IST day to fetch, YYYY-MM-DD (default: --from)")
	derived := fs.String("derive", "5m", "comma-separated intervals to build from the 1m candles; empty for none")
	dataDir := fs.String("data-dir", "data", "root directory of the Parquet store")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: jevtrader backfill --tokens 408065,738561 --from 2026-09-01 [--to 2026-09-30] [flags]")
		fmt.Fprintln(stderr, "\nFetches 1m candles from Kite historical REST into the bars_backfill store.")
		fmt.Fprintln(stderr, "Reads KITE_API_KEY and KITE_ACCESS_TOKEN from the environment.")
		fmt.Fprintln(stderr, "\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	job, err := parseBackfill(*tokens, *from, *to, *derived, *dataDir)
	if err != nil {
		return err
	}

	apiKey, accessToken := env("KITE_API_KEY"), env("KITE_ACCESS_TOKEN")
	if apiKey == "" || accessToken == "" {
		return errors.New("KITE_API_KEY and KITE_ACCESS_TOKEN must be set (run jevtrader login for the daily access token)")
	}
	kite := kiteconnect.New(apiKey)
	kite.SetAccessToken(accessToken)
	if base := env("KITE_API_BASE_URL"); base != "" {
		kite.SetBaseURI(base)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	limiter := rate.NewLimiter(rate.Every(time.Second/historicalRate), 1)
	return job.run(ctx, kite, limiter, log)
}

func parseBackfill(tokens, from, to, derived, dataDir string) (backfillJob, error) {
	var job backfillJob
	for s := range strings.SplitSeq(tokens, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 {
			return job, fmt.Errorf("invalid instrument token %q", s)
		}
		if !slices.Contains(job.tokens, uint32(n)) {
			job.tokens = append(job.tokens, uint32(n))
		}
	}
	if len(job.tokens) == 0 {
		return job, errors.New("--tokens is required")
	}
	if from == "" {
		return job, errors.New("--from is required")
	}
	if to == "" {
		to = from
	}
	var err error
	if job.from, err = time.ParseInLocation(time.DateOnly, from, ist); err != nil {
		return job, fmt.Errorf("--from: %w", err)
	}
	if job.to, err = time.ParseInLocation(time.DateOnly, to, ist); err != nil {
		return job, fmt.Errorf("--to: %w", err)
	}
	if job.to.Before(job.from) {
		return job, errors.New("--to is before --from")
	}
	for s := range strings.SplitSeq(derived, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return job, fmt.Errorf("--derive: %w", err)
		}
		job.derived = append(job.derived, d)
	}
	if _, err := bars.New(bars.Config{Base: time.Minute, Derived: job.derived}); err != nil {
		return job, fmt.Errorf("--derive: %w", err)
	}
	job.dataDir = dataDir
	return job, nil
}

// run fetches the job in chunks of at most maxChunkDays and writes each chunk
// once every token in it has been fetched, so an interrupted run publishes
// only whole chunks.
//
// Backfill never appends to a day that is already stored: rerunning a range
// would duplicate bars. It refuses up front instead, and the operator removes
// the day's partition to fetch it again.
func (job backfillJob) run(ctx context.Context, kite historian, limiter *rate.Limiter, log *slog.Logger) error {
	if err := job.checkEmpty(); err != nil {
		return err
	}
	w, err := store.NewBarWriter(job.dataDir, store.KindBackfill)
	if err != nil {
		return err
	}
	for start := job.from; !start.After(job.to); start = start.AddDate(0, 0, maxChunkDays) {
		last := start.AddDate(0, 0, maxChunkDays-1)
		if last.After(job.to) {
			last = job.to
		}
		chunk, err := job.fetchChunk(ctx, kite, limiter, start, last.Add(24*time.Hour-time.Second))
		if err != nil {
			return errors.Join(err, w.Close())
		}
		if err := w.Write(chunk...); err != nil {
			return errors.Join(err, w.Close())
		}
		log.Info("backfilled", "from", start.Format(time.DateOnly), "to", last.Format(time.DateOnly), "bars", len(chunk))
	}
	return w.Close()
}

func (job backfillJob) checkEmpty() error {
	for day := job.from; !day.After(job.to); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(job.dataDir, store.KindBackfill, "date="+day.Format(time.DateOnly))
		files, err := filepath.Glob(filepath.Join(dir, "*.parquet"))
		if err != nil {
			return err
		}
		if len(files) > 0 {
			return fmt.Errorf("%s already holds backfilled bars; remove it to fetch the day again", dir)
		}
	}
	return nil
}

// fetchChunk returns the 1m candles and derived bars of every token between
// from and to, ordered like bars.Builder output.
func (job backfillJob) fetchChunk(ctx context.Context, kite historian, limiter *rate.Limiter, from, to time.Time) ([]domain.Bar, error) {
	var out []domain.Bar
	for _, token := range job.tokens {
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		candles, err := kite.GetHistoricalData(int(token), "minute", from, to, false, false)
		if err != nil {
			return nil, fmt.Errorf("historical data for %d from %s: %w", token, from.Format(time.DateOnly), err)
		}
		ones := make([]domain.Bar, 0, len(candles))
		for _, c := range candles {
			if c.Volume < 0 {
				return nil, fmt.Errorf("historical data for %d: negative volume at %v", token, c.Date.Time)
			}
			ones = append(ones, domain.Bar{
				InstrumentToken: token,
				Interval:        time.Minute,
				Start:           c.Date.Time,
				Open:            c.Open,
				High:            c.High,
				Low:             c.Low,
				Close:           c.Close,
				Volume:          uint64(c.Volume),
			})
		}
		out = append(out, ones...)
		for _, d := range job.derived {
			agg, err := bars.Aggregate(ones, d)
			if err != nil {
				return nil, err
			}
			out = append(out, agg...)
		}
	}
	bars.Sort(out)
	return out, nil
}

var _ historian = (*kiteconnect.Client)(nil)
