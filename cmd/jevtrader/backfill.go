package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/kiteauth"
	kitemd "github.com/sanjeev2040/jev-trader/internal/marketdata/kite"
	"github.com/sanjeev2040/jev-trader/internal/screen"
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
	tokens []uint32
	// symbols are EXCHANGE:SYMBOL keys resolved through the instrument master;
	// instruments holds what they and the tokens resolved to, so the stored
	// bars carry their symbol.
	symbols     []string
	instruments map[uint32]domain.Instrument
	from, to    time.Time // first and last IST day, at midnight
	derived     []time.Duration
	dataDir     string
}

func runBackfill(ctx context.Context, args []string, e env) int {
	if err := backfill(ctx, args, e); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader backfill: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// usageError marks a mistake in the command line rather than a failed run.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func backfill(ctx context.Context, args []string, e env) error {
	stderr := e.stderr
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tokens := fs.String("tokens", "", "comma-separated Kite instrument tokens")
	symbols := fs.String("symbols", "", `comma-separated EXCHANGE:SYMBOL keys, e.g. "NSE:INFY,NSE:NIFTY 50", resolved through the instrument master; stored bars then carry their symbol (--tokens and/or --symbols is required)`)
	stateDir := fs.String("state-dir", "", "directory of the saved Kite session (default $XDG_STATE_HOME/jevtrader or ~/.local/state/jevtrader)")
	from := fs.String("from", "", "first IST day to fetch, YYYY-MM-DD (required)")
	to := fs.String("to", "", "last IST day to fetch, YYYY-MM-DD (default: --from)")
	derived := fs.String("derive", "5m", "comma-separated intervals to build from the 1m candles; empty for none")
	dataDir := fs.String("data-dir", "data", "root directory of the Parquet store")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: jevtrader backfill --symbols NSE:INFY,NSE:TCS --from 2026-09-01 [--to 2026-09-30] [flags]")
		fmt.Fprintln(stderr, "\nFetches 1m candles from Kite historical REST into the bars_backfill store.")
		fmt.Fprintln(stderr, "Reads KITE_API_KEY from the environment, or from the Azure Key Vault named by")
		fmt.Fprintln(stderr, "JEVTRADER_KEYVAULT. The access token is KITE_ACCESS_TOKEN if set, otherwise the")
		fmt.Fprintln(stderr, "session saved by `jevtrader login`.")
		fmt.Fprintln(stderr, "\nflags:")
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
	job, err := parseBackfill(*tokens, *symbols, *from, *to, *derived, *dataDir)
	if err != nil {
		return usageError{err}
	}

	env := func(k string) string { v, _ := e.lookup(k); return v }
	secrets, err := loadSecrets(ctx, e, config.EnvKiteAPIKey)
	if err != nil {
		return usageError{err}
	}
	apiKey, accessToken := secrets.KiteAPIKey.Reveal(), env("KITE_ACCESS_TOKEN")
	if accessToken == "" && apiKey != "" {
		accessToken = savedAccessToken(ctx, *stateDir)
	}
	if apiKey == "" || accessToken == "" {
		return usageError{errors.New("KITE_API_KEY and KITE_ACCESS_TOKEN must be set (run jevtrader login for the daily access token)")}
	}
	kite := kiteconnect.New(apiKey)
	kite.SetAccessToken(accessToken)
	baseURL := env("KITE_API_BASE_URL")
	if baseURL != "" {
		kite.SetBaseURI(baseURL)
	}
	if err := job.resolveSymbols(ctx, time.Now(), kitemd.HTTPMasterFetcher(nil, baseURL, apiKey, staticToken(accessToken))); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	limiter := rate.NewLimiter(rate.Every(time.Second/historicalRate), 1)
	return job.run(ctx, kite, limiter, log)
}

// staticToken is a kitemd.TokenProvider for a token already in hand.
type staticToken string

func (t staticToken) Token(context.Context) (string, error) { return string(t), nil }

// savedAccessToken returns the access token `jevtrader login` saved, or "" if
// there is no valid session; the caller then reports the missing token.
func savedAccessToken(ctx context.Context, stateDir string) string {
	if stateDir == "" {
		var err error
		if stateDir, err = kiteauth.DefaultStateDir(); err != nil {
			return ""
		}
	}
	tok, err := (&kiteauth.Auth{Store: kiteauth.Store{Dir: stateDir}}).Token(ctx)
	if err != nil {
		return ""
	}
	return tok.Reveal()
}

func parseBackfill(tokens, symbols, from, to, derived, dataDir string) (backfillJob, error) {
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
	job.symbols = splitList(symbols)
	if len(job.tokens) == 0 && len(job.symbols) == 0 {
		return job, errors.New("--tokens or --symbols is required")
	}
	if _, err := kitemd.Exchanges(job.symbols); err != nil {
		return job, fmt.Errorf("--symbols: %w", err)
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

// resolveSymbols looks the job's symbols up in the instrument master, adds
// their tokens to the job and remembers the instruments so that bars carry
// their exchange and symbol. Tokens named without a symbol stay symbol-less.
func (job *backfillJob) resolveSymbols(ctx context.Context, now time.Time, fetch kitemd.MasterFetcher) error {
	if len(job.symbols) == 0 {
		return nil
	}
	exchanges, err := kitemd.Exchanges(job.symbols)
	if err != nil {
		return usageError{err}
	}
	master, err := kitemd.LoadMaster(ctx, job.dataDir, now, exchanges, fetch)
	if err != nil {
		return err
	}
	found, err := kitemd.Resolve(master, job.symbols)
	if err != nil {
		return usageError{err}
	}
	job.instruments = make(map[uint32]domain.Instrument, len(found))
	for _, in := range found {
		job.instruments[in.Token] = in
		if !slices.Contains(job.tokens, in.Token) {
			job.tokens = append(job.tokens, in.Token)
		}
	}
	return nil
}

// instrument returns what the job knows about a token.
func (job backfillJob) instrument(token uint32) domain.Instrument {
	if in, ok := job.instruments[token]; ok {
		return in
	}
	return domain.Instrument{Token: token}
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
		candles, err := screen.Retry(ctx, screen.RetryAttempts, screen.RetryBackoff, func() ([]kiteconnect.HistoricalData, error) {
			return kite.GetHistoricalData(int(token), "minute", from, to, false, false)
		})
		if err != nil {
			return nil, fmt.Errorf("historical data for %d from %s: %w", token, from.Format(time.DateOnly), err)
		}
		ones := make([]domain.Bar, 0, len(candles))
		for _, c := range candles {
			if c.Volume < 0 {
				return nil, fmt.Errorf("historical data for %d: negative volume at %v", token, c.Date.Time)
			}
			ones = append(ones, domain.Bar{
				Instrument: job.instrument(token),
				Interval:   time.Minute,
				Start:      c.Date.Time,
				End:        c.Date.Time.Add(time.Minute),
				Open:       c.Open,
				High:       c.High,
				Low:        c.Low,
				Close:      c.Close,
				Volume:     int64(c.Volume),
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
