package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"
	"golang.org/x/time/rate"

	"log/slog"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/study"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func TestStudyRange(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, watchlist.IST) // a Saturday
	day := func(d time.Time) string { return d.Format(time.DateOnly) }
	first, last, err := studyRange("2026-09-01", "", 0, now)
	if err != nil || day(first) != "2026-09-01" || day(last) != "2026-10-02" {
		t.Errorf("default to: %v %v %v", first, last, err)
	}
	first, last, err = studyRange("", "2026-10-02", 4, now)
	if err != nil || day(first) != "2026-09-05" || day(last) != "2026-10-02" {
		t.Errorf("weeks: %v %v %v", first, last, err)
	}
	for _, c := range []struct {
		from, to string
		weeks    int
	}{
		{"", "", 0}, {"2026-09-01", "", 2}, {"2026-09-01", "2026-10-03", 0}, {"2026-09-01", "2026-12-01", 0},
		{"2026-09-10", "2026-09-01", 0}, {"nope", "", 0}, {"", "", -1},
	} {
		if _, _, err := studyRange(c.from, c.to, c.weeks, now); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}

func TestFingerprintChangesWithWhatItCovers(t *testing.T) {
	a := fingerprint("run", []string{"jev"}, 50)
	if a != fingerprint("run", []string{"jev"}, 50) || a == fingerprint("run", []string{"jev"}, 51) || a == fingerprint("run", []string{"hold"}, 50) {
		t.Error("fingerprint does not follow its inputs")
	}
}

// studyKite serves 75 one-minute candles from 09:15 on each requested day: a
// stock whose price follows its token (rising or falling), unless it is listed
// as empty, and the index.
type studyKite struct {
	mu     sync.Mutex
	calls  map[int]int
	empty  map[int]bool
	closed map[string]bool // days with no candles at all
}

func (k *studyKite) GetHistoricalData(token int, _ string, from, _ time.Time, _, _ bool) ([]kiteconnect.HistoricalData, error) {
	k.mu.Lock()
	k.calls[token]++
	k.mu.Unlock()
	day := from.In(ist).Format(time.DateOnly)
	if k.empty[token] || k.closed[day] {
		return nil, nil
	}
	start := time.Date(from.In(ist).Year(), from.In(ist).Month(), from.In(ist).Day(), 9, 15, 0, 0, ist)
	slope := 0.0005
	if token%2 == 0 {
		slope = -0.0004
	}
	// Each day has its own price level, as it would: the snapshot Jev is asked
	// about has no date in it, so identical days would share their answers.
	level := 500 + float64(from.In(ist).Day())
	var out []kiteconnect.HistoricalData
	for i := range 75 {
		p := level * (1 + slope*float64(i))
		out = append(out, kiteconnect.HistoricalData{
			Date: models.Time{Time: start.Add(time.Duration(i) * time.Minute)}, Open: p, High: p + 1, Low: p - 1, Close: p * 1.0001, Volume: 1000 * (i + 1),
		})
	}
	return out, nil
}

func (k *studyKite) total() (n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.calls {
		n += c
	}
	return n
}

func studyMaster() map[string]domain.Instrument {
	m := map[string]domain.Instrument{"NSE:NIFTY 50": {Token: 256265, Exchange: "NSE", Symbol: "NIFTY 50"}}
	for i, s := range []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF"} {
		m["NSE:"+s] = domain.Instrument{Token: uint32(11 + i), Exchange: "NSE", Symbol: s}
	}
	return m
}

func newTestBackfiller(root string, k *studyKite) *studyBackfiller {
	byKey := studyMaster()
	return &studyBackfiller{
		root: root, kite: k, limiter: rate.NewLimiter(rate.Inf, 1), byKey: byKey, index: byKey["NSE:NIFTY 50"],
		log: slog.New(slog.DiscardHandler),
	}
}

func TestStudyBackfillerFetchesOnlyWhatIsNew(t *testing.T) {
	k := &studyKite{calls: map[int]int{}, empty: map[int]bool{14: true}}
	b := newTestBackfiller(t.TempDir(), k)
	ctx := context.Background()
	missing, err := b.fetch(ctx, "2026-09-28", []string{"NSE:AAA", "NSE:BBB", "NSE:DDD", "NSE:GONE"})
	if err != nil {
		t.Fatal(err)
	}
	// DDD has no candles, GONE is not in the instrument master.
	if !slices.Equal(missing, []string{"NSE:DDD", "NSE:GONE"}) {
		t.Errorf("missing = %v", missing)
	}
	if k.total() != 4 { // AAA, BBB, DDD and the index
		t.Errorf("%d requests", k.total())
	}
	// The same day again asks for nothing; a new name asks for that name only.
	missing, err = b.fetch(ctx, "2026-09-28", []string{"NSE:AAA", "NSE:BBB", "NSE:DDD", "NSE:GONE", "NSE:CCC"})
	if err != nil || k.total() != 5 || !slices.Equal(missing, []string{"NSE:DDD", "NSE:GONE"}) {
		t.Errorf("second fetch: %v, %d requests, missing %v", err, k.total(), missing)
	}
	// A fresh backfiller on the same root reads the manifest from disk.
	b2 := newTestBackfiller(b.root, k)
	if _, err := b2.fetch(ctx, "2026-09-28", []string{"NSE:AAA"}); err != nil || k.total() != 5 {
		t.Errorf("reopened: %v, %d requests", err, k.total())
	}
	// A day with no candles for the index is no session.
	k.closed = map[string]bool{"2026-09-29": true}
	if _, err := b.fetch(ctx, "2026-09-29", []string{"NSE:AAA"}); err != study.ErrNoData {
		t.Errorf("closed day: %v", err)
	}
}

func fakeJev(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{
			"action":{"type":"choice","choice":"BUY","probabilities":{"BUY":0.62,"SELL":0.08,"HOLD":0.30},"confidence":0.62},
			"size":{"type":"choice","choice":"S","probabilities":{"S":0.6,"M":0.25,"L":0.15},"confidence":0.6}},
			"usage":{"input_tokens":187,"output_tokens":0}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestReplayer(t *testing.T, root string, jevURL string) (*studyReplayer, func()) {
	t.Helper()
	log, err := decisionlog.Open(filepath.Join(root, "d.db"), decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	answers, err := study.OpenAnswerStore(filepath.Join(root, "answers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := studyMaster()
	return &studyReplayer{
		o: studyOptions{
			model: "jev-1.13.0", jevURL: jevURL, capital: config.Capital{Amount: 25000, Source: "test"},
			window:  engine.Window{Start: 9*time.Hour + 15*time.Minute, End: 9*time.Hour + 25*time.Minute},
			latency: time.Second, decideTimeout: 10 * time.Second, horizon: 5, lookahead: []time.Duration{5 * time.Minute},
		},
		root: root, byKey: byKey, index: byKey["NSE:NIFTY 50"], log: log, answers: answers,
		secrets: config.Secrets{JevAPIKey: config.NewSecret("test-key")},
	}, func() { answers.Close(); log.Close() }
}

func TestStudyReplayerAnswersJevFromTheStoreTheSecondTime(t *testing.T) {
	root := t.TempDir()
	if _, err := newTestBackfiller(root, &studyKite{calls: map[int]int{}}).fetch(context.Background(), "2026-09-28", []string{"NSE:AAA", "NSE:BBB"}); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	srv := fakeJev(t, &hits)
	r, closeAll := newTestReplayer(t, root, srv.URL)
	defer closeAll()
	req := study.ReplayRequest{Day: "2026-09-28", List: study.ListPicks, Keys: []string{"NSE:AAA", "NSE:BBB"}, Deciders: []string{"jev", "hold", "momentum"}}
	rep, usage, err := r.replay(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Runs) != 3 || rep.Comparison == nil || len(rep.Comparison.BuyHold.Rows) != 2 {
		t.Fatalf("report: %d runs, comparison %+v", len(rep.Runs), rep.Comparison)
	}
	// 10 minutes of decisions over two stocks, none of them cached.
	if usage.Calls != 20 || usage.CacheHits != 0 || usage.InputTokens != 20*187 || int(hits.Load()) != 20 {
		t.Errorf("first replay: usage %+v, %d requests", usage, hits.Load())
	}
	if rep.Runs[0].Decider != "jev" || rep.Runs[0].Buys == 0 || rep.Runs[0].HoldSubstitutions != 0 {
		t.Errorf("jev run = %+v", rep.Runs[0])
	}
	// The index is context and never traded or listed.
	for _, row := range rep.Comparison.BuyHold.Rows {
		if row.Instrument == "NSE:NIFTY 50" {
			t.Errorf("the index was bought and held")
		}
	}
	// Asking again is free, and gives the same answers.
	rep2, usage2, err := r.replay(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if usage2.Calls != 0 || usage2.CacheHits != 20 || usage2.SavedInputTokens != 20*187 || int(hits.Load()) != 20 {
		t.Errorf("second replay: usage %+v, %d requests", usage2, hits.Load())
	}
	if rep2.Runs[0].NetPnL != rep.Runs[0].NetPnL || rep2.Runs[0].Buys != rep.Runs[0].Buys {
		t.Errorf("a cached replay differs: %+v vs %+v", rep2.Runs[0], rep.Runs[0])
	}
}

func TestStudyRunsEndToEndOnFixtures(t *testing.T) {
	dataDir := t.TempDir()
	k := &studyKite{calls: map[int]int{}, closed: map[string]bool{"2026-09-30": true}}
	var hits atomic.Int64
	srv := fakeJev(t, &hits)
	var screens atomic.Int64
	old := buildStudyDeps
	t.Cleanup(func() { buildStudyDeps = old })
	buildStudyDeps = func(_ context.Context, _ env, o studyOptions, _ *slog.Logger) (study.Deps, func(), error) {
		studyDir := filepath.Join(o.dataDir, "study")
		if err := os.MkdirAll(studyDir, 0o755); err != nil {
			return study.Deps{}, nil, err
		}
		bf := newTestBackfiller(studyDir, k)
		o.jevURL = srv.URL
		log, err := decisionlog.Open(o.logPath, decisionlog.Options{Synchronous: "NORMAL"})
		if err != nil {
			return study.Deps{}, nil, err
		}
		answers, err := study.OpenAnswerStore(filepath.Join(studyDir, "jev-answers.jsonl"))
		if err != nil {
			return study.Deps{}, nil, err
		}
		byKey := studyMaster()
		rp := &studyReplayer{o: o, root: studyDir, byKey: byKey, index: byKey["NSE:NIFTY 50"], log: log, answers: answers,
			secrets: config.Secrets{JevAPIKey: config.NewSecret("k")}}
		return study.Deps{
			Screen: func(_ context.Context, day string) (watchlist.File, error) {
				screens.Add(1)
				f := watchlist.File{Version: 1, Date: day, Phase: watchlist.PhaseHistoric, Final: true}
				for i, s := range []string{"AAA", "BBB", "CCC"} {
					f.Entries = append(f.Entries, watchlist.Entry{Rank: i + 1, Key: "NSE:" + s, Symbol: s, Sector: "IT", Score: 0.5, Signals: map[string]float64{"sector": 0.4, "range": float64(i) / 2}})
				}
				for _, s := range []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF"} {
					f.Pool = append(f.Pool, watchlist.PoolEntry{Key: "NSE:" + s, Sector: "IT", Score: 0.2, Signals: map[string]float64{"sector": 0.1}})
				}
				return f, nil
			},
			Backfill: bf.fetch,
			Replay:   rp.replay,
		}, func() { answers.Close(); log.Close() }, nil
	}

	report := filepath.Join(t.TempDir(), "out", "study.md")
	args := []string{"study", "--from", "2026-09-28", "--to", "2026-10-02", "--data-dir", dataDir, "--calendar", "", "--capital", "25000",
		"--start", "09:15", "--end", "09:25", "--deciders", "jev,hold,momentum", "--random-deciders", "momentum", "--out", report}
	e, stdout, stderr := testEnv(nil)
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	md := string(body)
	// Monday to Friday, less Wednesday the 30th, which Kite has no candles for.
	for _, want := range []string{
		"# Picker backtest, 2026-09-28 to 2026-10-02", "4 trading day(s) studied, 1 skipped", "2026-09-30: Kite has no candles",
		"| jev | picks |", "| momentum | random |", "## Jev token cost", "09:15–09:25 IST",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// 4 days x 3 names x 10 minutes of Jev decisions.
	if int(hits.Load()) != 120 {
		t.Errorf("%d Jev requests, want 120", hits.Load())
	}
	var js map[string]any
	raw, err := os.ReadFile(strings.TrimSuffix(report, ".md") + ".json")
	if err != nil || json.Unmarshal(raw, &js) != nil {
		t.Fatalf("json report: %v", err)
	}
	if !strings.Contains(stdout.String(), "Studied 4 trading day(s), 1 skipped.") {
		t.Errorf("stdout = %q", stdout.String())
	}

	// A second run reads every day from the store: no screen, no Kite, no Jev.
	calls, screened := k.total(), screens.Load()
	e, _, stderr = testEnv(nil)
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("rerun exit %d: %s", code, stderr.String())
	}
	// The skipped day is tried again, but its manifest already says Kite had nothing.
	if k.total() != calls || screens.Load() != screened || int(hits.Load()) != 120 {
		t.Errorf("rerun: %d Kite requests (was %d), %d screens (was %d), %d Jev requests", k.total(), calls, screens.Load(), screened, hits.Load())
	}
	// --report-only calls nothing at all.
	e, _, stderr = testEnv(nil)
	buildStudyDeps = func(context.Context, env, studyOptions, *slog.Logger) (study.Deps, func(), error) {
		t.Error("--report-only connected to something")
		return study.Deps{}, func() {}, nil
	}
	if code := run(context.Background(), append(args, "--report-only"), e); code != 0 {
		t.Fatalf("report-only exit %d: %s", code, stderr.String())
	}
}

func TestStudyFlagErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no range":         {"study"},
		"future":           {"study", "--from", "2026-09-01", "--to", "2999-01-01"},
		"unknown decider":  {"study", "--weeks", "1", "--deciders", "oracle"},
		"jev-latest":       {"study", "--weeks", "1", "--model", "jev-latest"},
		"bad window":       {"study", "--weeks", "1", "--start", "10:00", "--end", "09:30"},
		"random both ways": {"study", "--weeks", "1", "--no-random", "--random-size", "10"},
		"stray argument":   {"study", "--weeks", "1", "extra"},
		"bad news source":  {"study", "--weeks", "1", "--news-sources", "twitter"},
		"missing calendar": {"study", "--weeks", "1", "--calendar", "/no/such/holidays.yaml"},
	} {
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), args, e); code != 2 {
			t.Errorf("%s: exit %d, stderr %q", name, code, stderr.String())
		}
	}
}
