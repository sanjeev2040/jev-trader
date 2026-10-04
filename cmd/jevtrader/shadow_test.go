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
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/shadow"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func TestShadowPicksFollowTheWatchlistFile(t *testing.T) {
	infy := domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}
	tcs := domain.Instrument{Token: 2, Exchange: "NSE", Symbol: "TCS"}
	nifty := domain.Instrument{Token: 99, Exchange: "NSE", Symbol: "NIFTY 50"}
	f := &watchlist.File{Entries: []watchlist.Entry{
		{Rank: 1, Key: "NSE:TCS", Score: 0.9, News: []watchlist.NewsRef{{Title: "order win", Impact: 3, Material: 0.8, Direction: 0.9}}},
		{Rank: 2, Key: "NSE:INFY", Score: 0.4},
	}}
	// Instruments follow the keys, with the index after them.
	picks := shadowPicks(f, []string{"NSE:INFY", "NSE:TCS"}, []domain.Instrument{infy, tcs, nifty})
	if len(picks) != 2 || picks[0].Instrument != infy || picks[1].Instrument != tcs {
		t.Fatalf("picks = %+v", picks)
	}
	var d shadow.PickDetail
	if err := json.Unmarshal(picks[1].Detail, &d); err != nil || d.Rank != 1 || d.News == nil || d.News.Title != "order win" {
		t.Errorf("TCS detail = %s, %v", picks[1].Detail, err)
	}
	// A static watchlist has no picker behind it; a key that is not an entry is skipped.
	if got := shadowPicks(nil, []string{"NSE:INFY"}, []domain.Instrument{infy}); got != nil {
		t.Errorf("static list picks = %+v", got)
	}
	if got := shadowPicks(f, []string{"NSE:WIPRO"}, []domain.Instrument{infy}); len(got) != 0 {
		t.Errorf("unlisted key picks = %+v", got)
	}
}

// A paper session with the shadow log on writes every signal beside its cycle,
// and `shadow-report` fills the outcomes from the bars the same session
// recorded and prints them.
func TestShadowSessionThenReport(t *testing.T) {
	dir := t.TempDir()
	rc := tradeConfig{
		runID: "paper-shadow", mode: modePaper, source: sliceSource(fixtureTicks()), sourceName: "fixture",
		decider: decide.AlwaysHold{}, deciderName: "hold",
		logPath: filepath.Join(dir, "state", "decisions.db"), dataDir: filepath.Join(dir, "data"), stateDir: dir,
		indexToken: 99, record: true, buffer: 1024, decideTimeout: time.Second,
		limits: risk.DefaultLimits(), clock: clock.Wall{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		shadow: true, picks: []engine.ShadowPick{{Instrument: domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}, Detail: json.RawMessage(`{"rank":1}`)}},
	}
	if err := tradeSession(context.Background(), rc); err != nil {
		t.Fatal(err)
	}

	log, err := decisionlog.Open(rc.logPath, decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := log.Cycles(context.Background(), "paper-shadow")
	sigs, _ := log.ShadowSignals(context.Background(), "2026-10-05", "2026-10-05")
	log.Close()
	picks := 0
	for _, s := range sigs {
		if s.Kind == decisionlog.ShadowPick {
			picks++
			// The limits' entry start and end become the pick time and the window end.
			if !s.RefTime.Equal(testDay.Add(9*time.Hour+20*time.Minute)) || !s.WindowEnd.Equal(testDay.Add(15*time.Hour)) {
				t.Errorf("pick = %+v", s)
			}
		}
	}
	if picks != 1 || len(sigs) != len(cs)+1 {
		t.Fatalf("%d signals (%d picks) for %d cycles", len(sigs), picks, len(cs))
	}

	args := []string{"shadow-report", "--from", "2026-10-05", "--log", rc.logPath, "--data-dir", rc.dataDir}
	e, stdout, stderr := testEnv(nil)
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Shadow log 2026-10-05 to 2026-10-05", "decision:HOLD", "pick", "+15m", "n/a (needs 2 days)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "outcomes written") {
		t.Errorf("stderr = %q", stderr.String())
	}

	// The outcomes are in the log now: a report without --fill reads the same.
	e2, stdout2, _ := testEnv(nil)
	if code := run(context.Background(), append(args, "--fill=false", "--json"), e2); code != 0 {
		t.Fatalf("no-fill code %d", code)
	}
	var rep shadow.Report
	if err := json.Unmarshal([]byte(stdout2.String()), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout2.String())
	}
	if rep.Signals != len(sigs) || len(rep.Rows) == 0 || rep.Benchmark != "index" {
		t.Errorf("report = %+v", rep)
	}
}

func TestShadowReportValidatesItsFlags(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "decisions.db")
	l, err := decisionlog.Open(logPath, decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	for name, args := range map[string][]string{
		"no from":        {"shadow-report"},
		"bad from":       {"shadow-report", "--from", "yesterday"},
		"to before from": {"shadow-report", "--from", "2026-10-05", "--to", "2026-10-01"},
		"bad benchmark":  {"shadow-report", "--from", "2026-10-05", "--log", logPath, "--benchmark", "nifty"},
		"bad resamples":  {"shadow-report", "--from", "2026-10-05", "--resamples", "0"},
		"stray argument": {"shadow-report", "--from", "2026-10-05", "extra"},
	} {
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), args, e); code != 2 {
			t.Errorf("%s: code %d, stderr %q; want 2", name, code, stderr.String())
		}
	}
	// A log that is not there is an error, and no empty log is created in its place.
	missing := filepath.Join(dir, "nope.db")
	e, _, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"shadow-report", "--from", "2026-10-05", "--log", missing}, e); code != 1 {
		t.Errorf("missing log: code %d, stderr %q; want 1", code, stderr.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("shadow-report created a decision log")
	}
	// An empty log reports that nothing is in range.
	e, stdout, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"shadow-report", "--from", "2026-10-05", "--log", logPath, "--data-dir", dir}, e); code != 0 || !strings.Contains(stdout.String(), "no outcomes in range") {
		t.Errorf("empty log: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
