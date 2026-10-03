package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/kiteauth"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

// fakeKiteDay serves the instrument master (from the kite package's fixture)
// and 30 one-minute candles from 09:15 on the requested day. INFY rises, and
// the index (token 256265) rises with it.
func fakeKiteDay(t *testing.T) *httptest.Server {
	t.Helper()
	master, err := os.ReadFile(filepath.Join("..", "..", "internal", "marketdata", "kite", "testdata", "instruments_nse.csv"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token key:secret-token" {
			http.Error(w, `{"status":"error","error_type":"TokenException","message":"bad token"}`, http.StatusForbidden)
			return
		}
		if r.URL.Path == "/instruments/NSE" {
			_, _ = w.Write(master)
			return
		}
		var token int
		if _, err := fmt.Sscanf(r.URL.Path, "/instruments/historical/%d/minute", &token); err != nil {
			http.NotFound(w, r)
			return
		}
		start, _ := time.ParseInLocation(time.DateOnly, r.URL.Query().Get("from")[:10], ist)
		start = start.Add(9*time.Hour + 15*time.Minute)
		base := 1500.0
		if token == 256265 {
			base = 24000
		}
		var candles [][]any
		for i := range 30 {
			p := base * (1 + 0.0005*float64(i))
			ts := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04:05-0700")
			candles = append(candles, []any{ts, p, p + 1, p - 1, p, 1000 * (i + 1)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"candles": candles}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBackfillSymbolsThenReplayTheSession(t *testing.T) {
	srv := fakeKiteDay(t)
	dir := t.TempDir()
	vars := map[string]string{"KITE_API_KEY": "key", "KITE_ACCESS_TOKEN": "secret-token", "KITE_API_BASE_URL": srv.URL}

	e, _, stderr := testEnv(vars)
	args := []string{"backfill", "--symbols", "NSE:INFY, NSE:NIFTY 50", "--from", "2026-10-05", "--derive", "", "--data-dir", dir}
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("backfill exit %d: %s", code, stderr.String())
	}
	got, err := store.ReadBars(dir, store.KindBackfill, store.BarQuery{Interval: time.Minute})
	if err != nil || len(got) != 60 {
		t.Fatalf("stored %d bars, %v", len(got), err)
	}
	symbols := map[string]bool{}
	for _, b := range got {
		symbols[b.Instrument.Exchange+":"+b.Instrument.Symbol] = true
	}
	if !symbols["NSE:INFY"] || !symbols["NSE:NIFTY 50"] || len(symbols) != 2 {
		t.Errorf("stored instruments = %v", symbols)
	}

	// Replay 09:20-09:30 with the walkthrough and the comparison. --compare
	// brings the hold baseline; the index is found by its symbol.
	e, stdout, stderr := testEnv(nil)
	replayArgs := []string{"replay", "--data-dir", dir, "--log", filepath.Join(t.TempDir(), "d.db"), "--source", "backfill",
		"--from", "2026-10-05", "--to", "2026-10-05", "--start", "09:20", "--end", "09:30", "--deciders", "momentum",
		"--walkthrough", "all", "--compare", "--lookahead", "5m,10m", "--format", "json", "--run-prefix", "s"}
	if code := run(context.Background(), replayArgs, e); code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "also replays the hold baseline") || strings.Contains(stderr.String(), "no index context") {
		t.Errorf("stderr = %q", stderr.String())
	}
	var rep replay.Report
	if err := json.Unmarshal([]byte(stdout.String()), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout.String())
	}
	if rep.Window != "09:20-09:30 IST" || len(rep.Runs) != 2 || rep.Runs[0].Cycles != 10 {
		t.Fatalf("report = window %q, runs %+v", rep.Window, rep.Runs)
	}
	if len(rep.Walkthrough) != 2 || rep.Comparison == nil || fmt.Sprint(rep.Comparison.LookaheadMinutes) != "[5 10]" {
		t.Fatalf("walkthrough %d, comparison %+v", len(rep.Walkthrough), rep.Comparison)
	}
	iw := rep.Walkthrough[0].Instruments
	if len(iw) != 1 || iw[0].Instrument != "NSE:INFY" || len(iw[0].Rows) == 0 || iw[0].Rows[0].Features == nil || iw[0].Rows[0].Features.IndexSessionPct == nil {
		t.Fatalf("walkthrough = %+v", iw)
	}
	// 09:21 is a decision on the bar 09:20: five warm-up bars come before it.
	if f := iw[0].Rows[0].Features; f.MinutesSinceOpen != 6 || f.Return5Pct == nil {
		t.Errorf("first features = %+v", f)
	}
	if len(rep.Comparison.Calls) == 0 || len(rep.Comparison.BuyHold.Rows) != 1 {
		t.Errorf("comparison = %+v", rep.Comparison)
	}
	// The comparison looks past the window: the data runs to 09:44.
	last := rep.Comparison.Calls[len(rep.Comparison.Calls)-1]
	if o := last.Outcomes[1]; o.Label != "+10m" || o.Price == nil {
		t.Errorf("last call outcomes = %+v", last.Outcomes)
	}

	// Text carries both sections too.
	e, stdout, _ = testEnv(nil)
	textArgs := append(append([]string(nil), replayArgs[:len(replayArgs)-4]...), "--format", "text", "--run-prefix", "t")
	if code := run(context.Background(), textArgs, e); code != 0 {
		t.Fatalf("text replay exit %d", code)
	}
	for _, want := range []string{"Walkthrough: momentum", "Comparison with the real market", "Buy-and-hold benchmark"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("text lacks %q", want)
		}
	}
}

func TestReplayIndexByTokenForSymbollessBackfill(t *testing.T) {
	srv := fakeKiteDay(t)
	dir := t.TempDir()
	vars := map[string]string{"KITE_API_KEY": "key", "KITE_ACCESS_TOKEN": "secret-token", "KITE_API_BASE_URL": srv.URL}
	e, _, stderr := testEnv(vars)
	if code := run(context.Background(), []string{"backfill", "--tokens", "408065,256265", "--from", "2026-10-05", "--data-dir", dir}, e); code != 0 {
		t.Fatalf("backfill exit %d: %s", code, stderr.String())
	}
	replayJSON := func(extra ...string) (replay.Report, string) {
		e, stdout, stderr := testEnv(nil)
		args := append([]string{"replay", "--data-dir", dir, "--log", filepath.Join(t.TempDir(), "d.db"), "--source", "backfill",
			"--deciders", "momentum", "--walkthrough", "all", "--format", "json"}, extra...)
		if code := run(context.Background(), args, e); code != 0 {
			t.Fatalf("replay exit %d: %s", code, stderr.String())
		}
		var rep replay.Report
		if err := json.Unmarshal([]byte(stdout.String()), &rep); err != nil {
			t.Fatal(err)
		}
		return rep, stderr.String()
	}

	// Without symbols the default index cannot be found: say so.
	rep, warn := replayJSON()
	if !strings.Contains(warn, "no index context") {
		t.Errorf("no warning about the missing index: %q", warn)
	}
	if f := rep.Walkthrough[0].Instruments[0].Rows[0].Features; f == nil || f.IndexSessionPct != nil {
		t.Errorf("features without an index = %+v", f)
	}
	// By token it is, and it is not traded; rows name the token for lack of a symbol.
	rep, warn = replayJSON("--index", "256265")
	if warn != "" {
		t.Errorf("stderr = %q", warn)
	}
	insts := rep.Walkthrough[0].Instruments
	if len(insts) != 1 || insts[0].Instrument != "token 408065" || insts[0].Rows[0].Features.IndexSessionPct == nil {
		t.Errorf("walkthrough = %+v", insts)
	}
}

func TestBackfillUsesTheSavedLoginSession(t *testing.T) {
	srv := fakeKiteDay(t)
	state := t.TempDir()
	save := func(expires time.Time) {
		err := kiteauth.Store{Dir: state}.Save(kiteauth.Session{
			UserID: "AB1234", AccessToken: config.NewSecret("secret-token"), LoginTime: time.Now().Add(-time.Hour), ExpiresAt: expires,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	backfill := func() (int, string) {
		e, _, stderr := testEnv(map[string]string{"KITE_API_KEY": "key", "KITE_API_BASE_URL": srv.URL})
		args := []string{"backfill", "--tokens", "408065", "--from", "2026-10-05", "--derive", "", "--data-dir", t.TempDir(), "--state-dir", state}
		return run(context.Background(), args, e), stderr.String()
	}
	save(time.Now().Add(-time.Minute))
	if code, out := backfill(); code != 2 || !strings.Contains(out, "KITE_ACCESS_TOKEN") {
		t.Errorf("expired session: exit %d: %s", code, out)
	}
	save(time.Now().Add(time.Hour))
	if code, out := backfill(); code != 0 {
		t.Errorf("valid session: exit %d: %s", code, out)
	}
}

func TestKeyVaultFeedsLocalCommands(t *testing.T) {
	var calls []string
	old := azRunner
	t.Cleanup(func() { azRunner = old })
	azRunner = func(_ context.Context, args ...string) ([]byte, error) {
		name := args[len(args)-5] // ... --name <name> --query value -o tsv
		calls = append(calls, name)
		if name == "jev-api-key" {
			return []byte("vault-jev-secret\n"), nil
		}
		return nil, errors.New("az failed: not found")
	}

	// A replay that asks for jev reads jev-api-key from the vault; the model
	// is unreachable, so every answer is a substituted HOLD, and the run
	// still finishes without printing the key.
	root := writeFixtureTicks(t)
	e, stdout, stderr := testEnv(map[string]string{config.EnvKeyVault: "kv-jev-test"})
	args := []string{"replay", "--data-dir", root, "--log", filepath.Join(t.TempDir(), "d.db"), "--deciders", "jev",
		"--jev-url", "http://127.0.0.1:1", "--decide-timeout", "200ms", "--run-prefix", "v"}
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr.String())
	}
	if fmt.Sprint(calls) != "[jev-api-key]" {
		t.Errorf("vault reads = %v, want only jev-api-key", calls)
	}
	if strings.Contains(stdout.String()+stderr.String(), "vault-jev-secret") {
		t.Error("the secret was printed")
	}

	// The vault wins over the environment, and no vault means no az call.
	calls = nil
	e, _, _ = testEnv(map[string]string{"JEV_API_KEY": "env-key"})
	args = []string{"replay", "--data-dir", root, "--log", filepath.Join(t.TempDir(), "d.db"), "--deciders", "hold"}
	if code := run(context.Background(), args, e); code != 0 || len(calls) != 0 {
		t.Errorf("without a vault: exit %d, az calls %v", code, calls)
	}

	// login needs the Kite pair; with the vault unreadable and no environment
	// it says what failed.
	e, _, stderr = testEnv(map[string]string{config.EnvKeyVault: "kv-jev-test"})
	if code := run(context.Background(), []string{"login"}, e); code != 1 || !strings.Contains(stderr.String(), "kv-jev-test") || !strings.Contains(stderr.String(), "kite-api-key") {
		t.Errorf("login with an unreadable vault: exit %d: %s", code, stderr.String())
	}
	if fmt.Sprint(calls) != "[kite-api-key kite-api-secret]" {
		t.Errorf("login vault reads = %v", calls)
	}
}

func TestReplayWindowAndOptionsAreValidated(t *testing.T) {
	root := t.TempDir()
	for name, args := range map[string][]string{
		"bad start":         {"--start", "9am"},
		"bad end":           {"--end", "25:00"},
		"end before start":  {"--start", "10:00", "--end", "09:30"},
		"end equals start":  {"--start", "10:00", "--end", "10:00"},
		"bad walkthrough":   {"--walkthrough", "some"},
		"bad lookahead":     {"--lookahead", "90s"},
		"fractional minute": {"--lookahead", "5m30s"},
		"junk lookahead":    {"--lookahead", "soon"},
	} {
		e, _, stderr := testEnv(nil)
		full := append([]string{"replay", "--data-dir", root, "--log", filepath.Join(t.TempDir(), "d.db")}, args...)
		if code := run(context.Background(), full, e); code != 2 {
			t.Errorf("%s: code %d, stderr %q; want 2", name, code, stderr.String())
		}
	}
	if w, err := parseWindow("09:20", ""); err != nil || w.Start != 9*time.Hour+20*time.Minute || w.End != 0 {
		t.Errorf("start only = %+v, %v", w, err)
	}
	if w, err := parseWindow("", "10:15"); err != nil || w.Start != 0 || w.End != 10*time.Hour+15*time.Minute {
		t.Errorf("end only = %+v, %v", w, err)
	}
}
