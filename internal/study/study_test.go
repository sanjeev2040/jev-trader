package study

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/nsecal"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestStats(t *testing.T) {
	if r, ok := pearson([]float64{1, 2, 3, 4}, []float64{2, 4, 6, 8}); !ok || !near(r, 1) {
		t.Errorf("pearson of a line = %v %v", r, ok)
	}
	if _, ok := pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Error("pearson with no spread is defined")
	}
	if _, ok := pearson([]float64{1, 2}, []float64{1, 2}); ok {
		t.Error("pearson of two pairs is defined")
	}
	// Monotone but not linear: Spearman 1, Pearson below 1.
	xs, ys := []float64{1, 2, 3, 4, 5}, []float64{1, 4, 9, 16, 100}
	if rho, ok := spearman(xs, ys); !ok || !near(rho, 1) {
		t.Errorf("spearman = %v %v", rho, ok)
	}
	if r, _ := pearson(xs, ys); r >= 1 {
		t.Errorf("pearson of a curve = %v", r)
	}
	if got := ranks([]float64{10, 20, 20, 30}); !slices.Equal(got, []float64{1, 2.5, 2.5, 4}) {
		t.Errorf("ranks with ties = %v", got)
	}
	if got := tStat([]float64{1, 2, 3}); !near(got, 2/(1/math.Sqrt(3))) {
		t.Errorf("t = %v", got)
	}
	if tStat([]float64{5}) != 0 || tStat([]float64{2, 2}) != 0 {
		t.Error("t of a single or constant series is not 0")
	}
}

func TestDays(t *testing.T) {
	first, _ := time.ParseInLocation(time.DateOnly, "2026-10-01", watchlist.IST) // Thursday
	last, _ := time.ParseInLocation(time.DateOnly, "2026-10-06", watchlist.IST)  // Tuesday
	days, closed := Days(first, last, nil)
	if !slices.Equal(days, []string{"2026-10-01", "2026-10-02", "2026-10-05", "2026-10-06"}) {
		t.Errorf("days = %v", days)
	}
	if closed["2026-10-03"] != "weekend" {
		t.Errorf("closed = %v", closed)
	}
	cal, err := nsecal.Parse(strings.NewReader("years_complete: [2026]\nholidays:\n  - {date: \"2026-10-02\", name: Gandhi Jayanti, verified: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	days, closed = Days(first, last, cal)
	if !slices.Equal(days, []string{"2026-10-01", "2026-10-05", "2026-10-06"}) || !strings.Contains(closed["2026-10-02"], "Gandhi") {
		t.Errorf("days %v closed %v", days, closed)
	}
}

func pool(keys ...string) []watchlist.PoolEntry {
	var out []watchlist.PoolEntry
	for _, k := range keys {
		out = append(out, watchlist.PoolEntry{Key: k})
	}
	return out
}

func TestSample(t *testing.T) {
	var keys []string
	for i := range 40 {
		keys = append(keys, fmt.Sprintf("NSE:S%02d", i))
	}
	exclude := keys[:10]
	a := Sample(pool(keys...), exclude, 12, 7, "2026-10-01")
	b := Sample(pool(slices.Clone(keys)...), exclude, 12, 7, "2026-10-01")
	if len(a) != 12 || fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("not reproducible or wrong size: %d", len(a))
	}
	for _, p := range a {
		if slices.Contains(exclude, p.Key) {
			t.Errorf("%s is excluded", p.Key)
		}
	}
	if c := Sample(pool(keys...), exclude, 12, 7, "2026-10-02"); fmt.Sprint(a) == fmt.Sprint(c) {
		t.Error("another day drew the same list")
	}
	if c := Sample(pool(keys...), exclude, 12, 8, "2026-10-01"); fmt.Sprint(a) == fmt.Sprint(c) {
		t.Error("another seed drew the same list")
	}
	if got := Sample(pool(keys...), keys[:35], 12, 7, "2026-10-01"); len(got) != 5 {
		t.Errorf("a short pool gave %d", len(got))
	}
}

func TestCachedDecider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "answers.jsonl")
	store, err := OpenAnswerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	inner := deciderFunc(func(_ context.Context, state json.RawMessage) (domain.Decision, error) {
		calls++
		if string(state) == `"bad"` {
			return domain.Decision{}, errors.New("down")
		}
		return domain.Decision{
			Action: domain.ActionBuy, Size: "M", Probability: 0.7, Confidence: 0.5, Model: "jev-1.13.0",
			Probabilities: map[domain.Action]float64{domain.ActionBuy: 0.7, domain.ActionHold: 0.3},
			InputTokens:   100, OutputTokens: 5, Latency: 300 * time.Millisecond, Raw: json.RawMessage(`{"x":1}`),
		}, nil
	})
	c := &CachedDecider{Inner: inner, Model: "jev-1.13.0", Store: store}
	ctx := context.Background()
	first, err := c.Decide(ctx, json.RawMessage(`{"a":1}`))
	if err != nil || calls != 1 {
		t.Fatalf("first: %v, calls %d", err, calls)
	}
	again, err := c.Decide(ctx, json.RawMessage(`{"a":1}`))
	if err != nil || calls != 1 {
		t.Fatalf("again: %v, calls %d", err, calls)
	}
	if again.Action != first.Action || again.Probability != 0.7 || again.Latency != 300*time.Millisecond ||
		again.Probabilities[domain.ActionHold] != 0.3 || string(again.Raw) != `{"x":1}` {
		t.Errorf("cached decision differs: %+v", again)
	}
	if _, err := c.Decide(ctx, json.RawMessage(`"bad"`)); err == nil {
		t.Fatal("a failure was swallowed")
	}
	if _, err := c.Decide(ctx, json.RawMessage(`"bad"`)); err == nil || calls != 3 {
		t.Fatalf("a failure was cached (calls %d)", calls)
	}
	u := c.TakeUsage()
	if u.Calls != 1 || u.CacheHits != 1 || u.InputTokens != 100 || u.SavedInputTokens != 100 || u.OutputTokens != 5 {
		t.Errorf("usage = %+v", u)
	}
	if got := c.TakeUsage(); got != (Usage{}) {
		t.Errorf("usage was not reset: %+v", got)
	}
	// Another model's answers do not count.
	other := &CachedDecider{Inner: inner, Model: "jev-2", Store: store}
	if _, err := other.Decide(ctx, json.RawMessage(`{"a":1}`)); err != nil || calls != 4 {
		t.Errorf("another model hit the cache (calls %d)", calls)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// A new process finds the answers, and survives a line cut short.
	store, err = OpenAnswerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Len() != 2 {
		t.Errorf("reopened store holds %d answers", store.Len())
	}
	c = &CachedDecider{Inner: inner, Model: "jev-1.13.0", Store: store}
	if _, err := c.Decide(ctx, json.RawMessage(`{"a":1}`)); err != nil || calls != 4 {
		t.Errorf("reopened store missed (calls %d)", calls)
	}
}

type deciderFunc func(context.Context, json.RawMessage) (domain.Decision, error)

func (f deciderFunc) Decide(ctx context.Context, s json.RawMessage) (domain.Decision, error) {
	return f(ctx, s)
}

type countingMarket struct {
	mu    sync.Mutex
	calls map[string]int
}

func (m *countingMarket) Daily(_ context.Context, key string, from, to time.Time) ([]screen.Candle, error) {
	m.mu.Lock()
	m.calls[key]++
	m.mu.Unlock()
	var out []screen.Candle
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		out = append(out, screen.Candle{Date: d.Format(time.DateOnly), Close: 100})
	}
	return out, nil
}

func (m *countingMarket) Quotes(context.Context, []string) (map[string]screen.Quote, error) {
	return nil, errors.New("not called")
}

func TestRangeMarketFetchesEachStockOnce(t *testing.T) {
	inner := &countingMarket{calls: map[string]int{}}
	first, _ := time.ParseInLocation(time.DateOnly, "2026-10-05", watchlist.IST)
	last, _ := time.ParseInLocation(time.DateOnly, "2026-10-09", watchlist.IST)
	m := NewRangeMarket(inner, first, last, 45)
	ctx := context.Background()
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		cs, err := m.Daily(ctx, "NSE:A", d.AddDate(0, 0, -45), d.Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if n := len(cs); n != 45 {
			t.Fatalf("%s: %d candles, want 45", d.Format(time.DateOnly), n)
		}
		if got := cs[len(cs)-1].Date; got != d.AddDate(0, 0, -1).Format(time.DateOnly) {
			t.Errorf("%s: newest candle %s", d.Format(time.DateOnly), got)
		}
	}
	if inner.calls["NSE:A"] != 1 {
		t.Errorf("inner called %d times", inner.calls["NSE:A"])
	}
	// A request the range does not cover is passed through.
	if _, err := m.Daily(ctx, "NSE:A", first.AddDate(0, 0, -100), first.AddDate(0, 0, -50)); err != nil || inner.calls["NSE:A"] != 2 {
		t.Errorf("outside the range: %v, calls %d", err, inner.calls["NSE:A"])
	}
}

// fixture is the fake Deps of a study of three stocks and a pool of eight.
type fixture struct {
	mu                          sync.Mutex
	screens, backfills, replays int
	noData                      map[string]bool
	missing                     []string
	replayed                    []ReplayRequest
}

func (f *fixture) deps() Deps {
	return Deps{
		Screen: func(_ context.Context, day string) (watchlist.File, error) {
			f.mu.Lock()
			f.screens++
			f.mu.Unlock()
			file := watchlist.File{Version: 1, Date: day, Phase: watchlist.PhaseHistoric, Final: true,
				Sources: []watchlist.Source{{Name: "jev", Kind: "jev", Status: watchlist.StatusOK, InputTokens: 1000, OutputTokens: 10}}}
			for i, k := range []string{"NSE:A", "NSE:B", "NSE:C"} {
				sig := map[string]float64{screen.SignalSector: 0.5, screen.SignalRange: float64(i) / 2}
				if i == 0 {
					sig[screen.SignalNews] = 0.8
				}
				file.Entries = append(file.Entries, watchlist.Entry{Rank: i + 1, Key: k, Symbol: k[4:], Sector: "IT", Score: 0.6 - float64(i)/10, Signals: sig})
			}
			file.Pool = []watchlist.PoolEntry{{Key: "NSE:A"}, {Key: "NSE:B"}, {Key: "NSE:C"}}
			for _, k := range []string{"NSE:D", "NSE:E", "NSE:F", "NSE:G", "NSE:H"} {
				file.Pool = append(file.Pool, watchlist.PoolEntry{Key: k, Sector: "Realty", Score: 0.1, Signals: map[string]float64{screen.SignalSector: -0.4, screen.SignalRange: 0.1}})
			}
			return file, nil
		},
		Backfill: func(_ context.Context, day string, _ []string) ([]string, error) {
			f.mu.Lock()
			f.backfills++
			f.mu.Unlock()
			if f.noData[day] {
				return nil, ErrNoData
			}
			return f.missing, nil
		},
		Replay: func(_ context.Context, req ReplayRequest) (*replay.Report, Usage, error) {
			f.mu.Lock()
			f.replays++
			f.replayed = append(f.replayed, req)
			f.mu.Unlock()
			return fakeReport(req), Usage{Calls: 10, CacheHits: 2, InputTokens: 5000, OutputTokens: 50, SavedInputTokens: 900}, nil
		},
	}
}

// fakeReport gives every stock a return that grows with its key, and each
// decider a P&L that depends on the decider and the day.
func fakeReport(req ReplayRequest) *replay.Report {
	day := int(req.Day[len(req.Day)-1] - '0')
	rep := &replay.Report{Horizon: 5, Comparison: &replay.Comparison{}}
	for i, d := range req.Deciders {
		net := 0.0
		switch d {
		case "jev":
			net = float64(10 * day)
		case "momentum":
			net = -5
		}
		m := replay.RunMetrics{
			RunID: req.Day + "-" + d, Decider: d, NetPnL: net, GrossPnL: net + 3, Costs: 3, Buys: 4, Sells: 3, Holds: 20 + i,
			Trades: 2, Wins: 1, Calibration: replay.Calibration{Samples: 4, Brier: 0.2, Bins: []replay.Bin{{Lo: 0.6, Hi: 0.7, N: 4, Predicted: 0.65, Realized: 0.5}}},
		}
		rep.Runs = append(rep.Runs, m)
		rep.Comparison.Summary = append(rep.Comparison.Summary, replay.CallSummary{RunID: m.RunID, Label: "+5m", Calls: 4, Scored: 4, Right: 3, MeanMovePct: 0.1})
	}
	bh := &rep.Comparison.BuyHold
	for i, k := range req.Keys {
		ret := float64(i) - 1
		if req.List == ListRandom {
			ret = -0.5
		}
		dayClose := 100 * (1 + 2*ret/100)
		bh.Rows = append(bh.Rows, replay.BuyHoldRow{Day: req.Day, Instrument: k, StartPrice: 100, EndPrice: 100 * (1 + ret/100), DayClose: &dayClose, ReturnPct: ret, Net: ret * 10})
		bh.Net += ret * 10
		bh.Gross += ret*10 + 1
		bh.Costs++
	}
	return rep
}

func baseConfig(t *testing.T) Config {
	return Config{
		From: "2026-10-05", To: "2026-10-07", Dir: t.TempDir(),
		PickDeciders: []string{"jev", "momentum"}, RandomDeciders: []string{"momentum"}, Seed: 1,
		ScreenKey: "screen1", RunKey: "run1",
	}
}

func TestRunStudiesEachDayOnceAndCachesIt(t *testing.T) {
	cfg := baseConfig(t)
	f := &fixture{}
	days, err := Run(context.Background(), cfg, f.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 || f.screens != 3 || f.backfills != 3 || f.replays != 6 {
		t.Fatalf("days %d, screens %d, backfills %d, replays %d", len(days), f.screens, f.backfills, f.replays)
	}
	d := days[0]
	if d.Status != StatusOK || d.Picks.Names != 3 || d.Random == nil || d.Random.Names != 3 {
		t.Fatalf("day = %+v", d)
	}
	for _, r := range f.replayed {
		if r.List == ListRandom {
			for _, k := range r.Keys {
				if slices.Contains([]string{"NSE:A", "NSE:B", "NSE:C"}, k) {
					t.Errorf("random list holds pick %s", k)
				}
			}
		}
	}
	if d.ScreenInputTokens != 1000 || d.Jev.Calls != 20 {
		t.Errorf("tokens %d, usage %+v", d.ScreenInputTokens, d.Jev)
	}
	if d.Picks.BuyHold.NamesWithData != 3 || d.Picks.BuyHold.NamesUp != 1 || !near(d.Picks.BuyHold.MeanReturnPct, 0) {
		t.Errorf("picks buy-and-hold = %+v", d.Picks.BuyHold)
	}
	if len(d.Stocks) != 6 {
		t.Fatalf("%d stocks", len(d.Stocks))
	}
	a := d.Stocks[0]
	if a.Key != "NSE:A" || !a.Picked || a.Rank != 1 || a.Signals[screen.SignalNews] != 0.8 || a.DayReturnPct == nil || !near(*a.DayReturnPct, -2) {
		t.Errorf("first stock = %+v", a)
	}

	// A second run reads everything from the store.
	again, err := Run(context.Background(), cfg, f.deps())
	if err != nil || len(again) != 3 || f.screens != 3 || f.replays != 6 {
		t.Fatalf("rerun: %v, %d days, screens %d, replays %d", err, len(again), f.screens, f.replays)
	}
	// A different run key reuses the stored watchlists, and replays.
	cfg2 := cfg
	cfg2.RunKey = "run2"
	if _, err := Run(context.Background(), cfg2, f.deps()); err != nil || f.screens != 3 || f.replays != 12 {
		t.Fatalf("new run key: %v, screens %d, replays %d", err, f.screens, f.replays)
	}
	// A different screen key screens again.
	cfg3 := cfg
	cfg3.RunKey, cfg3.ScreenKey = "run3", "screen2"
	if _, err := Run(context.Background(), cfg3, f.deps()); err != nil || f.screens != 6 {
		t.Fatalf("new screen key: %v, screens %d", err, f.screens)
	}
	// Refresh recomputes.
	cfg.Refresh = true
	if _, err := Run(context.Background(), cfg, f.deps()); err != nil || f.screens != 9 {
		t.Fatalf("refresh: %v, screens %d", err, f.screens)
	}
	// CachedOnly calls nothing.
	cfg4 := cfg
	cfg4.Refresh, cfg4.CachedOnly = false, true
	screens, replays := f.screens, f.replays
	got, err := Run(context.Background(), cfg4, f.deps())
	if err != nil || len(got) != 3 || f.screens != screens || f.replays != replays {
		t.Fatalf("cached only: %v, %d days", err, len(got))
	}
	cfg4.RunKey = "never-run"
	if got, err := Run(context.Background(), cfg4, f.deps()); err != nil || len(got) != 0 {
		t.Errorf("cached only with nothing stored: %v, %d days", err, len(got))
	}
}

func TestRunSkipsDaysWithoutDataAndDropsMissingNames(t *testing.T) {
	cfg := baseConfig(t)
	f := &fixture{noData: map[string]bool{"2026-10-06": true}, missing: []string{"NSE:B", "NSE:E"}}
	days, err := Run(context.Background(), cfg, f.deps())
	if err != nil {
		t.Fatal(err)
	}
	if days[1].Status != StatusSkipped || !strings.Contains(days[1].Reason, "no candles") {
		t.Errorf("no-data day = %+v", days[1])
	}
	if days[0].Picks.Names != 3 || !slices.Equal(days[0].Picks.Missing, []string{"NSE:B"}) || days[0].Picks.BuyHold.NamesWithData != 2 {
		t.Errorf("missing name not dropped: %+v", days[0].Picks)
	}
	for _, r := range f.replayed {
		if slices.Contains(r.Keys, "NSE:B") || slices.Contains(r.Keys, "NSE:E") {
			t.Errorf("a missing name was replayed: %v", r.Keys)
		}
	}
	// The skipped day is tried again; the others are stored.
	n := f.backfills
	if _, err := Run(context.Background(), cfg, f.deps()); err != nil || f.backfills != n+1 {
		t.Errorf("rerun: %v, backfills %d (was %d)", err, f.backfills, n)
	}
}

func TestRunWithoutARandomList(t *testing.T) {
	cfg := baseConfig(t)
	cfg.RandomN = -1
	f := &fixture{}
	days, err := Run(context.Background(), cfg, f.deps())
	if err != nil || days[0].Random != nil || f.replays != 3 {
		t.Fatalf("%v, random %v, replays %d", err, days[0].Random, f.replays)
	}
}

func TestRunReportsFailures(t *testing.T) {
	cfg := baseConfig(t)
	f := &fixture{}
	d := f.deps()
	d.Replay = func(context.Context, ReplayRequest) (*replay.Report, Usage, error) {
		return nil, Usage{}, errors.New("jev unreachable")
	}
	if _, err := Run(context.Background(), cfg, d); err == nil || !strings.Contains(err.Error(), "2026-10-05") || !strings.Contains(err.Error(), "jev unreachable") {
		t.Errorf("err = %v", err)
	}
	// A screen that finds nothing skips the day; an expired session stops the study.
	d = f.deps()
	cfg.ScreenKey = "screen-fresh" // day one's watchlist is stored under the first key
	d.Screen = func(context.Context, string) (watchlist.File, error) {
		return watchlist.File{}, errors.New("no stock scored above 0.05")
	}
	days, err := Run(context.Background(), cfg, d)
	if err != nil || len(days) != 3 || days[0].Status != StatusSkipped || !strings.Contains(days[0].Reason, "no stock scored") {
		t.Errorf("screen failure: %v %+v", err, days)
	}
	d.Screen = func(context.Context, string) (watchlist.File, error) { return watchlist.File{}, screen.ErrAbort }
	if _, err := Run(context.Background(), cfg, d); !errors.Is(err, screen.ErrAbort) {
		t.Errorf("abort: %v", err)
	}
	bad := cfg
	bad.To = "2026-10-01"
	if _, err := Run(context.Background(), bad, f.deps()); err == nil {
		t.Error("a range that ends before it starts ran")
	}
	weekend := cfg
	weekend.From, weekend.To = "2026-10-03", "2026-10-04"
	if _, err := Run(context.Background(), weekend, f.deps()); err == nil {
		t.Error("a weekend ran")
	}
}

func TestSummarizeAndReport(t *testing.T) {
	cfg := baseConfig(t)
	f := &fixture{noData: map[string]bool{"2026-10-06": true}}
	days, err := Run(context.Background(), cfg, f.deps())
	if err != nil {
		t.Fatal(err)
	}
	s := Summarize(cfg.From, cfg.To, days)
	if s.OKDays != 2 || len(s.Skipped) != 1 {
		t.Fatalf("days ok %d skipped %d", s.OKDays, len(s.Skipped))
	}
	// Picks: returns -1, 0, +1 each day, nets -10, 0, +10 -> 0 a day.
	if s.Picks.Days != 2 || !near(s.Picks.Net, 0) || s.Picks.Names != 6 || s.Picks.NamesUp != 2 {
		t.Errorf("picks = %+v", s.Picks)
	}
	// Random: three stocks at -0.5%: net -15 a day.
	if !near(s.Random.Net, -30) || s.Random.NamesUp != 0 {
		t.Errorf("random = %+v", s.Random)
	}
	var jev DeciderSummary
	for _, d := range s.Deciders {
		if d.Decider == "jev" {
			jev = d
		}
	}
	// Oct 5 and Oct 7 end in digits 5 and 7: 50 + 70.
	if !near(jev.NetPnL, 120) || jev.Days != 2 || jev.PositiveDays != 2 || jev.Trades != 4 || jev.Wins != 2 || !near(jev.HitRate, 0.5) {
		t.Errorf("jev = %+v", jev)
	}
	paired := map[string]Paired{}
	for _, p := range s.Paired {
		paired[p.Name] = p
	}
	if p := paired["picks buy-and-hold − random buy-and-hold"]; p.N != 2 || !near(p.Total, 30) || p.Wins != 2 {
		t.Errorf("picks − random = %+v", p)
	}
	if p := paired["jev on picks − picks buy-and-hold"]; p.N != 2 || !near(p.Total, 120) {
		t.Errorf("jev − bh = %+v", p)
	}
	if _, ok := paired["momentum on picks − momentum on random"]; !ok {
		t.Errorf("no momentum picks − random: %v", paired)
	}
	if len(s.Calibration) == 0 || s.Calibration[0].Samples != 8 || !near(s.Calibration[0].Brier, 0.2) || len(s.Calibration[0].Bins) != 1 || s.Calibration[0].Bins[0].N != 8 {
		t.Errorf("calibration = %+v", s.Calibration)
	}
	var calls CallsSummary
	for _, c := range s.Calls {
		if c.Decider == "jev" {
			calls = c
		}
	}
	if calls.Scored != 8 || calls.Right != 6 || !near(calls.HitRate, 0.75) {
		t.Errorf("calls = %+v", calls)
	}
	if s.Usage.Calls != 40 || s.Usage.InputTokens != 20000 || s.ScreenInputTokens != 2000 {
		t.Errorf("usage %+v, screen tokens %d", s.Usage, s.ScreenInputTokens)
	}

	// Signals: within the picks, range 0, .5, 1 goes with returns -1, 0, 1.
	var rangePicks, newsAll SignalStat
	for _, st := range s.Signals {
		switch {
		case st.Signal == screen.SignalRange && st.Population == "picks":
			rangePicks = st
		case st.Signal == screen.SignalNews && st.Population == "all studied stocks":
			newsAll = st
		}
	}
	if rangePicks.N != 6 || !rangePicks.HasCorr || !near(rangePicks.Spearman, 1) {
		t.Errorf("range on picks = %+v", rangePicks)
	}
	if newsAll.N != 2 || newsAll.Above.N != 2 || !near(newsAll.Above.MeanReturnPct, -1) || newsAll.None.N != 10 {
		t.Errorf("news on all = %+v", newsAll)
	}
	if rangePicks.ICDays != 0 {
		t.Errorf("a day of three names counted for the daily IC: %+v", rangePicks)
	}

	var md bytes.Buffer
	if err := WriteMarkdown(&md, s, Meta{Generated: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Horizon: 5, Settings: [][2]string{{"Window", "09:15–10:15 IST"}}}); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{
		"# Picker backtest, 2026-10-05 to 2026-10-07", "2 trading day(s) studied, 1 skipped",
		"| Window | 09:15–10:15 IST |", "## Results", "| buy-and-hold | picks |", "| jev | picks | +120 |",
		"picks buy-and-hold − random buy-and-hold", "## Hit rates", "## Per day", "| 2026-10-05 |", "## Calibration",
		"**jev**: 8 calls, Brier 0.200", "## Which signals went with the window return", "## Jev token cost",
		"| Decisions, paid | 40 | 20000 |", "## Skipped days", "2026-10-06: Kite has no candles", "## Limitations", "Index constituents are today's",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	var js bytes.Buffer
	if err := WriteJSON(&js, s); err != nil || !json.Valid(js.Bytes()) {
		t.Errorf("json: %v", err)
	}
	// An empty study still reports.
	md.Reset()
	if err := WriteMarkdown(&md, Summarize("a", "b", nil), Meta{}); err != nil || !strings.Contains(md.String(), "No day could be studied") {
		t.Errorf("empty report: %v\n%s", err, md.String())
	}
}
