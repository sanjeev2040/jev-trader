package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/clock"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

var testDay = time.Date(2026, 10, 5, 0, 0, 0, 0, risk.IST)

var fixturePrices = []float64{
	1500, 1503, 1506, 1509, 1512, 1515, 1518, 1517, 1520, 1522, 1523,
	1520, 1515, 1510, 1505, 1500, 1498, 1497, 1496, 1495, 1494,
}

func fixtureTicks() []domain.Tick {
	var out []domain.Tick
	for i, p := range fixturePrices {
		ts := testDay.Add(9*time.Hour + time.Duration(15+i)*time.Minute + 30*time.Second).UTC()
		for _, in := range []struct {
			token uint32
			sym   string
			scale float64
		}{{99, "NSE:NIFTY 50", 10}, {1, "NSE:INFY", 1}} {
			out = append(out, domain.Tick{
				InstrumentToken: in.token, Symbol: in.sym, ExchangeTime: ts, ReceivedAt: ts.Add(40 * time.Millisecond),
				LastPrice: p * in.scale, Volume: int64(1000 * (i + 1)),
			})
		}
	}
	return out
}

// writeFixtureTicks records the fixture the way `jevtrader record` does.
func writeFixtureTicks(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	w := store.NewTickWriter(root, 0)
	for _, tk := range fixtureTicks() {
		if err := w.Write(tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunLiveModeNeverStarts(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--mode=live", "--watchlist", "NSE:INFY", "--model", "m"},
		{"run", "--mode=live", "--live-enabled", "--watchlist", "NSE:INFY", "--model", "m"},
		{"run", "--mode", "live", "--live-enabled"},
	} {
		// Even with every credential present, the gate refuses first.
		e, _, stderr := testEnv(map[string]string{
			"KITE_API_KEY": "k", "KITE_API_SECRET": "s", "JEV_API_KEY": "j", "KITE_ACCESS_TOKEN": "t",
		})
		code := run(context.Background(), args, e)
		if code == 0 || !strings.Contains(stderr.String(), "live trading is disabled") {
			t.Errorf("%v: code %d, stderr %q; want a refusal", args, code, stderr.String())
		}
	}
}

func TestRunValidatesInput(t *testing.T) {
	cases := map[string][]string{
		"unknown mode":      {"run", "--mode", "yolo", "--watchlist", "NSE:INFY"},
		"no watchlist":      {"run"},
		"stray argument":    {"run", "extra"},
		"non-positive buf":  {"run", "--watchlist", "NSE:INFY", "--buffer", "0"},
		"missing kite keys": {"run", "--watchlist", "NSE:INFY", "--model", "m"},
		"missing jev key":   {"run", "--watchlist", "NSE:INFY", "--model", "m"},
	}
	for name, args := range cases {
		vars := map[string]string{}
		if name == "missing jev key" {
			vars = map[string]string{"KITE_API_KEY": "k", "KITE_API_SECRET": "s"}
		}
		e, _, stderr := testEnv(vars)
		if code := run(context.Background(), args, e); code != 2 {
			t.Errorf("%s: code %d, stderr %q; want 2", name, code, stderr.String())
		}
	}
	e, _, stderr := testEnv(map[string]string{"KITE_API_KEY": "k", "KITE_API_SECRET": "s", "JEV_API_KEY": "j"})
	if code := run(context.Background(), []string{"run", "--watchlist", "NSE:INFY"}, e); code != 2 || !strings.Contains(stderr.String(), "--model") {
		t.Errorf("no model: code %d, stderr %q", code, stderr.String())
	}
}

type sliceSource []domain.Tick

func (s sliceSource) Run(ctx context.Context, out chan<- marketdata.Event) error {
	for i := range s {
		select {
		case out <- marketdata.Event{Tick: &s[i]}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestTradeSessionRecordsDecisionsTicksAndBars(t *testing.T) {
	dir := t.TempDir()
	rc := tradeConfig{
		runID: "paper-test", mode: modePaper, source: sliceSource(fixtureTicks()), sourceName: "fixture",
		decider: decide.AlwaysHold{}, deciderName: "hold",
		logPath: filepath.Join(dir, "state", "decisions.db"), dataDir: filepath.Join(dir, "data"), stateDir: dir,
		indexToken: 99, record: true, buffer: 1024, decideTimeout: time.Second,
		limits: risk.DefaultLimits(), clock: clock.Wall{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := tradeSession(context.Background(), rc); err != nil {
		t.Fatal(err)
	}

	log, err := decisionlog.Open(rc.logPath, decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	runs, _ := log.Runs(context.Background())
	if len(runs) != 1 || runs[0].ID != "paper-test" || runs[0].Mode != modePaper || runs[0].Source != "fixture" {
		t.Errorf("runs = %+v", runs)
	}
	cs, _ := log.Cycles(context.Background(), "paper-test")
	if len(cs) == 0 {
		t.Error("no decision cycles were logged")
	}
	for _, c := range cs {
		if c.Mode != modePaper || c.Instrument.Token == 99 {
			t.Errorf("cycle %+v", c)
		}
	}
	// Ticks recorded by the session replay as they came in.
	n := 0
	if err := store.ScanTicks(rc.dataDir, store.Query{}, func(domain.Tick) error { n++; return nil }); err != nil || n != len(fixtureTicks()) {
		t.Errorf("recorded %d ticks (%v), want %d", n, err, len(fixtureTicks()))
	}
	if bs, err := store.ReadBars(rc.dataDir, store.KindBars, store.BarQuery{}); err != nil || len(bs) == 0 {
		t.Errorf("recorded %d bars, %v", len(bs), err)
	}
}

func TestReplayCommandEndToEnd(t *testing.T) {
	root := writeFixtureTicks(t)
	logPath := filepath.Join(t.TempDir(), "nested", "decisions.db")
	base := []string{"replay", "--data-dir", root, "--log", logPath, "--format", "json"}

	e, stdout, stderr := testEnv(nil)
	if code := run(context.Background(), append(base, "--run-prefix", "cli", "--from", "2026-10-05", "--to", "2026-10-05"), e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var rep replay.Report
	if err := json.Unmarshal([]byte(stdout.String()), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, stdout.String())
	}
	if len(rep.Runs) != 2 || rep.Runs[0].RunID != "cli-hold" || rep.Runs[1].RunID != "cli-momentum" {
		t.Fatalf("runs = %+v", rep.Runs)
	}
	if hold, mom := rep.Runs[0], rep.Runs[1]; hold.Orders != 0 || hold.Cycles != 20 || mom.Orders != 2 || mom.Trades != 1 {
		t.Errorf("hold %+v\nmomentum %+v", hold, mom)
	}

	// A regression replay from the first run's log, as text.
	e, stdout, stderr = testEnv(nil)
	args := []string{"replay", "--data-dir", root, "--log", logPath, "--run-prefix", "again", "--deciders", "recorded:cli-momentum"}
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("recorded replay: code %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"again-recorded-cli-momentum", "net PnL", "optimistic upper bounds"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, stdout.String())
		}
	}

	// --out writes the report to a file.
	e, stdout, _ = testEnv(nil)
	outPath := filepath.Join(t.TempDir(), "report.txt")
	if code := run(context.Background(), []string{"replay", "--data-dir", root, "--log", logPath, "--run-prefix", "file", "--deciders", "hold", "--out", outPath}, e); code != 0 {
		t.Fatalf("--out: code %d", code)
	}
	if b, err := os.ReadFile(outPath); err != nil || !strings.Contains(string(b), "file-hold") || stdout.String() != "" {
		t.Errorf("report file = %q, %v; stdout %q", b, err, stdout.String())
	}
}

func TestReplayNeverTradesTheStoredIndex(t *testing.T) {
	root := writeFixtureTicks(t)
	traded := func(index ...string) bool {
		logPath := filepath.Join(t.TempDir(), "d.db")
		args := append([]string{"replay", "--data-dir", root, "--log", logPath, "--run-prefix", "x", "--deciders", "momentum"}, index...)
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), args, e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		log, err := decisionlog.Open(logPath, decisionlog.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		cs, _ := log.Cycles(context.Background(), "x-momentum")
		if len(cs) == 0 {
			t.Fatal("no cycles logged")
		}
		for _, c := range cs {
			if c.Instrument.Token == 99 {
				return true
			}
		}
		return false
	}
	if traded() {
		t.Error("default --index traded the NIFTY 50 index")
	}
	if traded("--tokens", "1") {
		t.Error("--tokens without the index traded it")
	}
	if !traded("--index", "") {
		t.Error("--index \"\" should leave the index an ordinary instrument")
	}
}

func TestReplayCommandValidatesInput(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(t.TempDir(), "d.db")
	for name, args := range map[string][]string{
		"bad source":       {"--source", "csv"},
		"bad date":         {"--from", "05/10/2026"},
		"bad token":        {"--tokens", "abc"},
		"bad format":       {"--format", "xml"},
		"unknown decider":  {"--deciders", "oracle"},
		"duplicate":        {"--deciders", "hold,hold"},
		"jev without key":  {"--deciders", "jev", "--model", "m"},
		"jev no model":     {"--deciders", "jev"},
		"recorded no id":   {"--deciders", "recorded"},
		"recorded unknown": {"--deciders", "recorded:nope"},
		"stray argument":   {"extra"},
	} {
		e, _, stderr := testEnv(map[string]string{"JEV_API_KEY": ""})
		full := append([]string{"replay", "--data-dir", root, "--log", log}, args...)
		if name == "jev no model" {
			e, _, stderr = testEnv(map[string]string{"JEV_API_KEY": "j"})
		}
		if code := run(context.Background(), full, e); code != 2 {
			t.Errorf("%s: code %d, stderr %q; want 2", name, code, stderr.String())
		}
	}
	// An empty store is a run with nothing to decide on, not a crash.
	e, _, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"replay", "--data-dir", root, "--log", log, "--run-prefix", "empty", "--deciders", "hold"}, e); code != 0 {
		t.Errorf("empty store: code %d, stderr %q", code, stderr.String())
	}
}

func TestListedCommands(t *testing.T) {
	e, _, stderr := testEnv(nil)
	run(context.Background(), []string{"help"}, e)
	for _, name := range []string{"run", "replay", "record", "login", "backfill"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("usage lacks %q:\n%s", name, stderr.String())
		}
	}
}
