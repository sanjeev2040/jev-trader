package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// screenMarket serves 25 sessions of candles ending the day before the
// screened day, whatever that day is, for the two fixture stocks.
type screenMarket struct{ dailyErr error }

func (m screenMarket) Daily(_ context.Context, key string, _, to time.Time) ([]screen.Candle, error) {
	if m.dailyErr != nil {
		return nil, m.dailyErr
	}
	base := map[string]float64{"NSE:KPITTECH": 1400, "NSE:BSOFT": 600, "NSE:CDSL": 1500}[key]
	var out []screen.Candle
	for i := 24; i >= 0; i-- {
		p := base
		if i == 0 {
			p = base * 1.02
		}
		out = append(out, screen.Candle{Date: to.AddDate(0, 0, -i).Format(time.DateOnly), Open: p, High: p * 1.015, Low: p * 0.985, Close: p, Volume: 1_000_000})
	}
	return out, nil
}

func (screenMarket) Quotes(context.Context, []string) (map[string]screen.Quote, error) {
	return map[string]screen.Quote{}, nil
}

const screenIndexCSV = "Company Name,Industry,Symbol,Series,ISIN Code\nKPIT Technologies Ltd.,Information Technology,KPITTECH,EQ,X\nBirlasoft Ltd.,Information Technology,BSOFT,EQ,X\nCentral Depository Services (India) Ltd.,Financial Services,CDSL,EQ,X\n"

func screenNSE(_ context.Context, name string) (io.ReadCloser, error) {
	docs := map[string]string{
		universe.Midcap150:   screenIndexCSV,
		universe.Smallcap250: "Company Name,Industry,Symbol,Series,ISIN Code\nTiny Ltd.,Realty,TINY,EQ,X\n",
		universe.Nifty50:     "Company Name,Industry,Symbol,Series,ISIN Code\nInfosys Ltd.,Information Technology,INFY,EQ,X\n",
		universe.NiftyNext50: "Company Name,Industry,Symbol,Series,ISIN Code\nABB India Ltd.,Capital Goods,ABB,EQ,X\n",
		"asm":                `{"longterm":{"data":[{"symbol":"ZZZ","survCode":"x"}]},"shortterm":{"data":[]}}`,
		"gsm":                `[{"symbol":"ZZZ","survCode":"x"}]`,
	}
	return io.NopCloser(strings.NewReader(docs[name])), nil
}

// useScreenFixtures swaps the real connections for fixtures for one test.
func useScreenFixtures(t *testing.T, market screen.Market) {
	t.Helper()
	old := buildScreenDeps
	t.Cleanup(func() { buildScreenDeps = old })
	buildScreenDeps = func(context.Context, env, screenOptions, *slog.Logger) (screen.Deps, error) {
		return screen.Deps{NSE: screenNSE, Market: market, Model: "jev-1.13.0"}, nil
	}
}

func todayIST() string { return time.Now().In(watchlist.IST).Format(time.DateOnly) }

func TestScreenWritesTheDatedWatchlist(t *testing.T) {
	useScreenFixtures(t, screenMarket{})
	out, data := t.TempDir(), t.TempDir()
	e, stdout, stderr := testEnv(nil)
	// A past day is always the historic phase, whatever the clock says.
	args := []string{"screen", "--date", "2020-01-02", "--out-dir", out, "--data-dir", data, "--news-sources", "none", "--top", "2", "--min-score", "0"}
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	f, err := watchlist.Read(filepath.Join(out, "2020-01-02.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Date != "2020-01-02" || f.Phase != watchlist.PhaseHistoric || !f.Final || len(f.Entries) != 2 {
		t.Errorf("file = date %s phase %s final %v entries %d", f.Date, f.Phase, f.Final, len(f.Entries))
	}
	for _, e := range f.Entries {
		if e.Key == "NSE:INFY" || e.Key == "NSE:ABB" || e.Key == "NSE:TINY" {
			t.Errorf("unexpected entry %s", e.Key)
		}
	}
	for _, want := range []string{"Watchlist for 2020-01-02", "NSE:", "Sectors:", "Sources:", "2020-01-02.json"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout.String())
		}
	}
	// The same file is what run reads.
	if got, err := watchlist.Read(filepath.Join(out, "2020-01-02.json")); err != nil || len(got.Keys()) != 2 {
		t.Errorf("read back: %v %v", got.Keys(), err)
	}
}

func TestEarlyScreenKeepsAFinalWatchlistUnlessForced(t *testing.T) {
	useScreenFixtures(t, screenMarket{})
	out := t.TempDir()
	path := watchlist.Path(out, todayIST())
	final := watchlist.File{Date: todayIST(), Phase: watchlist.PhaseFinal, Final: true, Direction: "long",
		Entries: []watchlist.Entry{{Rank: 1, Key: "NSE:KEEPME", Symbol: "KEEPME"}}}
	if err := watchlist.Write(path, final); err != nil {
		t.Fatal(err)
	}
	args := []string{"screen", "--phase", "early", "--out-dir", out, "--data-dir", t.TempDir(), "--news-sources", "none", "--min-score", "0"}

	e, stdout, stderr := testEnv(nil)
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already holds") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if got, _ := watchlist.Read(path); got.Entries[0].Key != "NSE:KEEPME" {
		t.Errorf("the final watchlist was replaced: %v", got.Keys())
	}

	e, _, stderr = testEnv(nil)
	if code := run(context.Background(), append(args, "--force"), e); code != 0 {
		t.Fatalf("--force: code %d, stderr %q", code, stderr.String())
	}
	if got, _ := watchlist.Read(path); got.Phase != watchlist.PhaseEarly || got.Final {
		t.Errorf("--force did not replace it: phase %s final %v", got.Phase, got.Final)
	}
}

func TestScreenReportsAnExpiredKiteSession(t *testing.T) {
	useScreenFixtures(t, screenMarket{dailyErr: fmt.Errorf("kite TokenException: %w", screen.ErrAbort)})
	e, _, stderr := testEnv(nil)
	args := []string{"screen", "--date", "2020-01-02", "--out-dir", t.TempDir(), "--data-dir", t.TempDir(), "--news-sources", "none"}
	if code := run(context.Background(), args, e); code != 1 || !strings.Contains(stderr.String(), "jevtrader login") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestScreenDepsErrorsAreReported(t *testing.T) {
	old := buildScreenDeps
	t.Cleanup(func() { buildScreenDeps = old })
	buildScreenDeps = func(context.Context, env, screenOptions, *slog.Logger) (screen.Deps, error) {
		return screen.Deps{}, usageError{errors.New("no Kite session")}
	}
	e, _, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"screen", "--out-dir", t.TempDir()}, e); code != 2 || !strings.Contains(stderr.String(), "no Kite session") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestScreenValidatesInput(t *testing.T) {
	useScreenFixtures(t, screenMarket{})
	tomorrow := time.Now().In(watchlist.IST).AddDate(0, 0, 1).Format(time.DateOnly)
	for name, args := range map[string][]string{
		"bad date":           {"--date", "05/10/2026"},
		"future date":        {"--date", tomorrow},
		"bad phase":          {"--phase", "later"},
		"final for the past": {"--date", "2020-01-02", "--phase", "final"},
		"bad direction":      {"--direction", "short"},
		"zero top":           {"--top", "0"},
		"all-zero weights":   {"--w-news", "0", "--w-sector", "0", "--w-gap", "0", "--w-range", "0"},
		"unknown source":     {"--news-sources", "nse-announcements,gossip"},
		"jev-latest":         {"--model", "jev-latest"},
		"stray argument":     {"extra"},
		"bad price band":     {"--min-price", "100", "--max-price", "50"},
	} {
		e, _, stderr := testEnv(nil)
		full := append([]string{"screen", "--out-dir", t.TempDir(), "--data-dir", t.TempDir()}, args...)
		if code := run(context.Background(), full, e); code != 2 {
			t.Errorf("%s: code %d, stderr %q; want 2", name, code, stderr.String())
		}
	}
}

func TestResolveScreenDay(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, watchlist.IST) }
	tests := []struct {
		name, date, phase string
		now               time.Time
		wantDay, want     string
		wantErr           bool
	}{
		{"auto before pre-open", "", "auto", at(8, 30), "2026-10-05", "early", false},
		{"auto during the call auction", "", "auto", at(9, 5), "2026-10-05", "early", false},
		{"auto after the equilibrium price", "", "auto", at(9, 8), "2026-10-05", "final", false},
		{"auto after the open", "2026-10-05", "auto", at(11, 0), "2026-10-05", "final", false},
		{"explicit early late", "", "early", at(10, 0), "2026-10-05", "early", false},
		{"explicit final early", "", "final", at(8, 0), "2026-10-05", "final", false},
		{"past day", "2026-10-02", "auto", at(8, 30), "2026-10-02", "historic", false},
		{"past day, historic named", "2026-10-02", "historic", at(8, 30), "2026-10-02", "historic", false},
		{"past day, early", "2026-10-02", "early", at(8, 30), "", "", true},
		{"future", "2026-10-06", "auto", at(8, 30), "", "", true},
		{"unknown phase", "", "soon", at(8, 30), "", "", true},
		{"garbage date", "yesterday", "auto", at(8, 30), "", "", true},
		{"IST day, not UTC day", "", "auto", time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC), "2026-10-05", "early", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			day, phase, err := resolveScreenDay(tt.date, tt.phase, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && (day.Format(time.DateOnly) != tt.wantDay || phase != tt.want) {
				t.Errorf("= %s %s, want %s %s", day.Format(time.DateOnly), phase, tt.wantDay, tt.want)
			}
		})
	}
}

func TestParseNewsSources(t *testing.T) {
	all, err := parseNewsSources("all")
	if err != nil || len(all) != 1+len(rssFeeds) || all[0] != nseAnnouncementsName {
		t.Errorf("all = %v, %v", all, err)
	}
	if none, err := parseNewsSources("none"); err != nil || len(none) != 0 {
		t.Errorf("none = %v, %v", none, err)
	}
	if some, err := parseNewsSources("nse-announcements, ndtvprofit"); err != nil || len(some) != 2 {
		t.Errorf("some = %v, %v", some, err)
	}
	if _, err := parseNewsSources("nope"); err == nil {
		t.Error("an unknown source was accepted")
	}
}

func TestScreenIsListed(t *testing.T) {
	e, _, stderr := testEnv(nil)
	run(context.Background(), []string{"help"}, e)
	if !strings.Contains(stderr.String(), "screen") {
		t.Errorf("usage lacks screen:\n%s", stderr.String())
	}
}

func TestResolveWatchlist(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	dir := t.TempDir()
	path := watchlist.Path(dir, "2026-10-05")
	if err := watchlist.Write(path, watchlist.File{Date: "2026-10-05", Phase: watchlist.PhaseFinal, Final: true,
		Entries: []watchlist.Entry{{Key: "NSE:KPITTECH"}, {Key: "NSE:BSOFT"}}}); err != nil {
		t.Fatal(err)
	}
	static := []string{"NSE:INFY"}
	onDay := time.Date(2026, 10, 5, 9, 14, 0, 0, watchlist.IST)

	t.Run("no file configured", func(t *testing.T) {
		got, err := resolveWatchlist(static, "", onDay, logger)
		if err != nil || strings.Join(got, ",") != "NSE:INFY" {
			t.Errorf("= %v, %v", got, err)
		}
	})
	t.Run("today's file wins", func(t *testing.T) {
		got, err := resolveWatchlist(static, path, onDay, logger)
		if err != nil || strings.Join(got, ",") != "NSE:KPITTECH,NSE:BSOFT" {
			t.Errorf("= %v, %v", got, err)
		}
		if !strings.Contains(logs.String(), "watchlist from file") {
			t.Errorf("log = %q", logs.String())
		}
	})
	t.Run("stale file falls back", func(t *testing.T) {
		got, err := resolveWatchlist(static, path, onDay.AddDate(0, 0, 1), logger)
		if err != nil || strings.Join(got, ",") != "NSE:INFY" {
			t.Errorf("= %v, %v", got, err)
		}
		if !strings.Contains(logs.String(), "not for today") {
			t.Errorf("the fallback was not explained: %q", logs.String())
		}
	})
	t.Run("missing file falls back", func(t *testing.T) {
		got, err := resolveWatchlist(static, filepath.Join(dir, "nope.json"), onDay, logger)
		if err != nil || strings.Join(got, ",") != "NSE:INFY" {
			t.Errorf("= %v, %v", got, err)
		}
	})
	t.Run("damaged file falls back", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		os.WriteFile(bad, []byte("{"), 0o644)
		got, err := resolveWatchlist(static, bad, onDay, logger)
		if err != nil || strings.Join(got, ",") != "NSE:INFY" {
			t.Errorf("= %v, %v", got, err)
		}
	})
	t.Run("no fallback", func(t *testing.T) {
		if _, err := resolveWatchlist(nil, filepath.Join(dir, "nope.json"), onDay, logger); err == nil {
			t.Error("a missing file with no static list was accepted")
		}
	})
}

func TestRunAcceptsAWatchlistFileInsteadOfAList(t *testing.T) {
	path := watchlist.Path(t.TempDir(), todayIST())
	if err := watchlist.Write(path, watchlist.File{Date: todayIST(), Entries: []watchlist.Entry{{Key: "NSE:KPITTECH"}}}); err != nil {
		t.Fatal(err)
	}
	e, _, stderr := testEnv(nil)
	code := run(context.Background(), []string{"run", "--watchlist-file", path}, e)
	// It gets past the watchlist check and stops at the missing Kite keys.
	if code != 2 || strings.Contains(stderr.String(), "--watchlist or --watchlist-file is required") || !strings.Contains(stderr.String(), "KITE") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
	e, _, stderr = testEnv(nil)
	if code := run(context.Background(), []string{"run"}, e); code != 2 || !strings.Contains(stderr.String(), "--watchlist or --watchlist-file is required") {
		t.Errorf("neither flag: code %d, stderr %q", code, stderr.String())
	}
}

func TestReplayWithAWatchlistFile(t *testing.T) {
	root := writeFixtureTicks(t) // NSE:INFY (token 1) and NSE:NIFTY 50 (token 99)
	dir := t.TempDir()
	write := func(name, date string, keys ...string) string {
		f := watchlist.File{Date: date, Phase: watchlist.PhaseFinal, Final: true}
		for i, k := range keys {
			f.Entries = append(f.Entries, watchlist.Entry{Rank: i + 1, Key: k})
		}
		p := filepath.Join(dir, name)
		if err := watchlist.Write(p, f); err != nil {
			t.Fatal(err)
		}
		return p
	}
	logPath := filepath.Join(t.TempDir(), "d.db")
	base := []string{"replay", "--data-dir", root, "--log", logPath, "--deciders", "hold", "--format", "json"}

	t.Run("limits the replay to the listed instruments and skips the unstored", func(t *testing.T) {
		p := write("a.json", "2026-10-05", "NSE:INFY", "NSE:GONE")
		e, stdout, stderr := testEnv(nil)
		if code := run(context.Background(), append(base, "--run-prefix", "wl", "--watchlist-file", p, "--from", "2026-10-05", "--to", "2026-10-05"), e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "NSE:GONE") || strings.Contains(stderr.String(), "outside the replayed days") {
			t.Errorf("stderr = %q", stderr.String())
		}
		if !strings.Contains(stdout.String(), `"cycles": 20`) {
			t.Errorf("INFY should have been replayed:\n%s", stdout.String())
		}
	})
	t.Run("warns about a watchlist for another day", func(t *testing.T) {
		p := write("b.json", "2026-10-06", "NSE:INFY")
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), append(base, "--run-prefix", "wl2", "--watchlist-file", p, "--from", "2026-10-05", "--to", "2026-10-05"), e); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "outside the replayed days") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})
	t.Run("nothing stored is an error", func(t *testing.T) {
		p := write("c.json", "2026-10-05", "NSE:GONE")
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), append(base, "--watchlist-file", p), e); code != 1 || !strings.Contains(stderr.String(), "none of the 1 instruments") {
			t.Errorf("code %d, stderr %q", code, stderr.String())
		}
	})
	t.Run("not with --tokens", func(t *testing.T) {
		p := write("d.json", "2026-10-05", "NSE:INFY")
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), append(base, "--watchlist-file", p, "--tokens", "1"), e); code != 2 {
			t.Errorf("code %d, stderr %q", code, stderr.String())
		}
	})
	t.Run("unreadable file", func(t *testing.T) {
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), append(base, "--watchlist-file", filepath.Join(dir, "nope.json")), e); code != 2 {
			t.Errorf("code %d, stderr %q", code, stderr.String())
		}
	})
}
