// Package study backtests the pre-market picker over many trading days. For
// each day it screens as of that morning, backfills the picks and a random
// list of the same size from the same screened pool, replays a window with the
// deciders, and keeps the day's result; Summarize and WriteMarkdown then
// aggregate the days. Every external system is behind Deps, so the whole
// thing runs on fixtures. See reports/0005-picker-study.md.
package study

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/nsecal"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// ResultVersion is the version of a day's stored result.
const ResultVersion = 1

// Day statuses.
const (
	StatusOK      = "ok"
	StatusSkipped = "skipped"
)

// ErrNoData is returned by Deps.Backfill when Kite holds no candles for the
// day, as on a holiday the calendar did not know of. The day is skipped.
var ErrNoData = errors.New("no candles for the day")

// List names, as passed to Deps.Replay.
const (
	ListPicks  = "picks"
	ListRandom = "random"
)

// Deps are the study's external systems.
type Deps struct {
	// Screen builds the watchlist for an IST day (YYYY-MM-DD) from what was
	// public before its open, keeping the screened pool in the file.
	Screen func(ctx context.Context, day string) (watchlist.File, error)
	// Backfill makes sure the day's 1m candles of the keys (and of the index)
	// are stored, fetching only what is missing. It returns the keys Kite has
	// no instrument or no candles for. ErrNoData means the market did not trade.
	Backfill func(ctx context.Context, day string, keys []string) (missing []string, err error)
	// Replay replays the day's window over the keys with the deciders, with
	// the market comparison, and says what the Jev decider cost.
	Replay func(ctx context.Context, req ReplayRequest) (*replay.Report, Usage, error)
}

// ReplayRequest is one replay of a day.
type ReplayRequest struct {
	Day      string
	List     string // ListPicks or ListRandom
	Keys     []string
	Deciders []string
}

// Config configures a study.
type Config struct {
	// From and To are the first and last IST day, YYYY-MM-DD, inclusive.
	From, To string
	// Dir is where the study keeps its watchlists and day results.
	Dir string
	// Calendar says which days trade; nil means every weekday does.
	Calendar *nsecal.Calendar
	// RandomN is how many stocks the random list holds; 0 means as many as
	// the day's picks, and a negative number leaves the random list out.
	RandomN int
	// Seed makes the random lists reproducible: the same seed, day and pool
	// give the same list.
	Seed int64
	// PickDeciders and RandomDeciders are the decider names replayed over each
	// list.
	PickDeciders, RandomDeciders []string
	// ScreenKey and RunKey identify what a stored watchlist and a stored day
	// result were computed with (weights, filters, window, deciders, model,
	// capital, ...). A change of either recomputes what depends on it.
	ScreenKey, RunKey string
	// Refresh ignores stored watchlists and day results.
	Refresh bool
	// CachedOnly uses stored day results only, calling no Deps: it rebuilds
	// the report of an earlier run.
	CachedOnly bool
	Logger     *slog.Logger
}

func (c Config) validate() (first, last time.Time, err error) {
	if first, err = time.ParseInLocation(time.DateOnly, c.From, watchlist.IST); err != nil {
		return first, last, fmt.Errorf("from: %w", err)
	}
	if last, err = time.ParseInLocation(time.DateOnly, c.To, watchlist.IST); err != nil {
		return first, last, fmt.Errorf("to: %w", err)
	}
	switch {
	case last.Before(first):
		return first, last, errors.New("to is before from")
	case c.Dir == "":
		return first, last, errors.New("a study directory is required")
	case len(c.PickDeciders) == 0:
		return first, last, errors.New("no deciders for the picks")
	case c.RunKey == "" || c.ScreenKey == "":
		return first, last, errors.New("screen and run keys are required")
	}
	return first, last, nil
}

// Days lists the trading days from first to last, with the reason for each day
// that is left out. Without a calendar every weekday trades.
func Days(first, last time.Time, cal *nsecal.Calendar) (days []string, closed map[string]string) {
	closed = map[string]string{}
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		date := d.Format(time.DateOnly)
		if cal == nil {
			if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
				closed[date] = "weekend"
				continue
			}
			days = append(days, date)
			continue
		}
		res, err := cal.Check(date)
		switch {
		case err != nil || !res.Open:
			closed[date] = res.Reason
		default:
			days = append(days, date)
		}
	}
	return days, closed
}

// Run studies every trading day of the range and returns the day results in
// date order, one per trading day: a day that could not be studied comes back
// skipped with its reason. A day already stored is not recomputed.
func Run(ctx context.Context, cfg Config, deps Deps) ([]DayResult, error) {
	first, last, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	days, _ := Days(first, last, cfg.Calendar)
	if len(days) == 0 {
		return nil, errors.New("no trading days in the range")
	}
	var out []DayResult
	for i, day := range days {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !cfg.Refresh {
			if r, ok := readResult(cfg, day); ok {
				cfg.Logger.Info("study day from cache", "day", day, "n", fmt.Sprintf("%d/%d", i+1, len(days)))
				out = append(out, r)
				continue
			}
		}
		if cfg.CachedOnly {
			continue
		}
		cfg.Logger.Info("study day", "day", day, "n", fmt.Sprintf("%d/%d", i+1, len(days)))
		r, err := runDay(ctx, cfg, deps, day)
		if err != nil {
			return out, fmt.Errorf("%s: %w", day, err)
		}
		// A skipped day is tried again next time: its cause may be temporary.
		if r.Status == StatusOK {
			if err := writeResult(cfg, r); err != nil {
				return out, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func skipped(day, why string) DayResult {
	return DayResult{Version: ResultVersion, Date: day, Status: StatusSkipped, Reason: why}
}

func runDay(ctx context.Context, cfg Config, deps Deps, day string) (DayResult, error) {
	wl, err := screenDay(ctx, cfg, deps, day)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, screen.ErrAbort) {
			return DayResult{}, err
		}
		// A day the screen cannot list (no liquid stock, nothing above the
		// minimum score) is a finding about the picker, not a failed study.
		cfg.Logger.Warn("screen produced no list", "day", day, "err", err)
		return skipped(day, "screen: "+err.Error()), nil
	}
	picks := wl.Keys()
	var random []watchlist.PoolEntry
	if cfg.RandomN >= 0 {
		n := cfg.RandomN
		if n == 0 {
			n = len(picks)
		}
		random = Sample(wl.Pool, picks, n, cfg.Seed, day)
	}
	keys := slices.Clone(picks)
	for _, p := range random {
		keys = append(keys, p.Key)
	}
	missing, err := deps.Backfill(ctx, day, keys)
	if errors.Is(err, ErrNoData) {
		return skipped(day, "Kite has no candles for the day"), nil
	}
	if err != nil {
		return DayResult{}, fmt.Errorf("backfill: %w", err)
	}

	res := DayResult{Version: ResultVersion, Date: day, Status: StatusOK, Phase: wl.Phase, Notes: wl.Notes}
	for _, s := range wl.Sources {
		res.ScreenInputTokens += s.InputTokens
		res.ScreenOutputTokens += s.OutputTokens
	}
	info := map[string]watchlist.PoolEntry{}
	for _, p := range wl.Pool {
		info[p.Key] = p
	}
	rank := map[string]int{}
	for _, e := range wl.Entries {
		rank[e.Key] = e.Rank
		info[e.Key] = watchlist.PoolEntry{Key: e.Key, Sector: e.Sector, Score: e.Score, Signals: e.Signals}
	}

	lists := []struct {
		name     string
		keys     []string
		deciders []string
	}{{ListPicks, picks, cfg.PickDeciders}}
	if len(random) > 0 && len(cfg.RandomDeciders) > 0 {
		rk := make([]string, len(random))
		for i, p := range random {
			rk[i] = p.Key
		}
		lists = append(lists, struct {
			name     string
			keys     []string
			deciders []string
		}{ListRandom, rk, cfg.RandomDeciders})
	}
	for _, l := range lists {
		var traded []string
		for _, k := range l.keys {
			if !slices.Contains(missing, k) {
				traded = append(traded, k)
			}
		}
		lr := ListResult{Names: len(l.keys)}
		for _, k := range l.keys {
			if slices.Contains(missing, k) {
				lr.Missing = append(lr.Missing, k)
			}
		}
		if len(traded) == 0 {
			res.setList(l.name, lr)
			continue
		}
		rep, usage, err := deps.Replay(ctx, ReplayRequest{Day: day, List: l.name, Keys: traded, Deciders: l.deciders})
		if err != nil {
			return DayResult{}, fmt.Errorf("replay %s: %w", l.name, err)
		}
		res.Jev.Add(usage)
		lr.fill(rep)
		res.setList(l.name, lr)
		if rep.Comparison != nil {
			for _, row := range rep.Comparison.BuyHold.Rows {
				pe := info[row.Instrument]
				sd := StockDay{
					Key: row.Instrument, Sector: pe.Sector, Picked: l.name == ListPicks, Rank: rank[row.Instrument],
					Score: pe.Score, Signals: pe.Signals, ReturnPct: row.ReturnPct,
				}
				if row.DayClose != nil && row.StartPrice > 0 {
					v := (*row.DayClose - row.StartPrice) / row.StartPrice * 100
					sd.DayReturnPct = &v
				}
				res.Stocks = append(res.Stocks, sd)
			}
		}
	}
	if res.Picks.Names > 0 && res.Picks.BuyHold.NamesWithData == 0 {
		return skipped(day, "no pick has candles"), nil
	}
	return res, nil
}

// screenDay returns the day's watchlist from the study's store, screening and
// storing it when it is not there.
func screenDay(ctx context.Context, cfg Config, deps Deps, day string) (watchlist.File, error) {
	path := watchlist.Path(filepath.Join(cfg.Dir, "watchlist", cfg.ScreenKey), day)
	if !cfg.Refresh {
		if f, err := watchlist.Read(path); err == nil && (len(f.Pool) > 0 || cfg.RandomN < 0) {
			return f, nil
		}
	}
	f, err := deps.Screen(ctx, day)
	if err != nil {
		return watchlist.File{}, err
	}
	if err := watchlist.Write(path, f); err != nil {
		return watchlist.File{}, err
	}
	return f, nil
}

// Sample draws n stocks of the pool that are not among exclude, uniformly and
// reproducibly from seed and day. It returns fewer if the pool has fewer.
func Sample(pool []watchlist.PoolEntry, exclude []string, n int, seed int64, day string) []watchlist.PoolEntry {
	var rest []watchlist.PoolEntry
	for _, p := range pool {
		if !slices.Contains(exclude, p.Key) {
			rest = append(rest, p)
		}
	}
	slices.SortFunc(rest, func(a, b watchlist.PoolEntry) int { return strings.Compare(a.Key, b.Key) })
	h := fnv.New64a()
	h.Write([]byte(day))
	rng := rand.New(rand.NewPCG(uint64(seed), h.Sum64()))
	rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	if n < len(rest) {
		rest = rest[:n]
	}
	return rest
}

func resultPath(cfg Config, day string) string {
	return filepath.Join(cfg.Dir, "days", cfg.RunKey, day+".json")
}

func readResult(cfg Config, day string) (DayResult, bool) {
	body, err := os.ReadFile(resultPath(cfg, day))
	if err != nil {
		return DayResult{}, false
	}
	var r DayResult
	if json.Unmarshal(body, &r) != nil || r.Version != ResultVersion || r.Date != day {
		return DayResult{}, false
	}
	return r, true
}

func writeResult(cfg Config, r DayResult) error {
	path := resultPath(cfg, r.Date)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+r.Date+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
