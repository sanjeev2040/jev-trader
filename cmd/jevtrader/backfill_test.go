package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"
	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

type request struct {
	token    int
	from, to string
}

// fakeKite serves Kite's historical endpoint: ten 1m candles from 09:15 on
// the first and last day of each requested range.
func fakeKite(t *testing.T) (*httptest.Server, func() []request) {
	t.Helper()
	var mu sync.Mutex
	var reqs []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token key:secret-token" {
			http.Error(w, `{"status":"error","error_type":"TokenException","message":"bad token"}`, http.StatusForbidden)
			return
		}
		var token int
		if _, err := fmt.Sscanf(r.URL.Path, "/instruments/historical/%d/minute", &token); err != nil {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		mu.Lock()
		reqs = append(reqs, request{token, q.Get("from"), q.Get("to")})
		mu.Unlock()
		var candles [][]any
		for _, day := range []string{q.Get("from")[:10], q.Get("to")[:10]} {
			start, _ := time.ParseInLocation(time.DateOnly, day, ist)
			start = start.Add(9*time.Hour + 15*time.Minute)
			for i := range 10 {
				ts := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04:05-0700")
				p := float64(token%1000) + float64(i)
				candles = append(candles, []any{ts, p, p + 2, p - 1, p + 1, 100 * (i + 1)})
			}
			if q.Get("from")[:10] == q.Get("to")[:10] {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"candles": candles}})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), reqs...)
	}
}

func TestBackfillCommand(t *testing.T) {
	srv, requests := fakeKite(t)
	dir := t.TempDir()
	vars := map[string]string{"KITE_API_KEY": "key", "KITE_ACCESS_TOKEN": "secret-token", "KITE_API_BASE_URL": srv.URL}
	args := []string{"backfill", "--tokens", "408065", "--from", "2026-09-01", "--to", "2026-09-02", "--data-dir", dir}

	e, _, stderr := testEnv(vars)
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "secret-token") {
		t.Errorf("access token logged: %s", stderr.String())
	}
	want := []request{{408065, "2026-09-01 00:00:00", "2026-09-02 23:59:59"}}
	if got := requests(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", got, want)
	}

	got, err := store.ReadBars(dir, store.KindBackfill, store.BarQuery{Interval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Each day has 09:15-09:24: two full 5m bars.
	if len(got) != 4 {
		t.Fatalf("got %d 5m bars, want 4: %+v", len(got), got)
	}
	first := got[0]
	wantBar := domain.Bar{Instrument: domain.Instrument{Token: 408065}, Interval: 5 * time.Minute, Open: 65, High: 71, Low: 64, Close: 70, Volume: 1500}
	if !first.Start.Equal(time.Date(2026, 9, 1, 9, 15, 0, 0, ist)) || !first.End.Equal(first.Start.Add(5*time.Minute)) {
		t.Errorf("first 5m window = [%v, %v)", first.Start, first.End)
	}
	first.Start, first.End = time.Time{}, time.Time{}
	if first != wantBar {
		t.Errorf("first 5m bar = %+v, want %+v", first, wantBar)
	}
	ones, err := store.ReadBars(dir, store.KindBackfill, store.BarQuery{Interval: time.Minute})
	if err != nil || len(ones) != 20 {
		t.Fatalf("got %d 1m bars (%v), want 20", len(ones), err)
	}

	// Rerunning the same days would duplicate bars, so it is refused before any request.
	e, _, stderr = testEnv(vars)
	if code := run(context.Background(), args, e); code != 1 || !strings.Contains(stderr.String(), "already holds") {
		t.Fatalf("rerun exit %d: %s", code, stderr.String())
	}
	if n := len(requests()); n != 1 {
		t.Errorf("rerun made %d more requests", n-1)
	}
}

func TestBackfillChunksLongRanges(t *testing.T) {
	srv, requests := fakeKite(t)
	kite := kiteconnect.New("key")
	kite.SetAccessToken("secret-token")
	kite.SetBaseURI(srv.URL)
	job, err := parseBackfill("2,1", "", "2026-01-01", "2026-03-15", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := job.run(context.Background(), kite, rate.NewLimiter(rate.Inf, 1), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	want := []request{
		{2, "2026-01-01 00:00:00", "2026-03-01 23:59:59"},
		{1, "2026-01-01 00:00:00", "2026-03-01 23:59:59"},
		{2, "2026-03-02 00:00:00", "2026-03-15 23:59:59"},
		{1, "2026-03-02 00:00:00", "2026-03-15 23:59:59"},
	}
	if got := requests(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("requests = %v\nwant       %v", got, want)
	}
	got, err := store.ReadBars(job.dataDir, store.KindBackfill, store.BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2*4*10 {
		t.Fatalf("got %d bars, want 80 1m bars and no derived bars", len(got))
	}
}

type failingKite struct{}

func (failingKite) GetHistoricalData(int, string, time.Time, time.Time, bool, bool) ([]kiteconnect.HistoricalData, error) {
	return nil, errors.New("boom")
}

func TestBackfillErrors(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	job, err := parseBackfill("1", "", "2026-09-01", "", "5m", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := job.run(context.Background(), failingKite{}, rate.NewLimiter(rate.Inf, 1), quiet); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("fetch error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := job.run(ctx, failingKite{}, rate.NewLimiter(1, 0), quiet); err == nil {
		t.Error("cancelled context did not stop the run")
	}

	e, _, stderr := testEnv(nil)
	if code := run(context.Background(), []string{"backfill", "--tokens", "1", "--from", "2026-09-01"}, e); code != 2 || !strings.Contains(stderr.String(), "KITE_API_KEY") {
		t.Errorf("missing secrets: exit %d: %s", code, stderr.String())
	}
	e, _, _ = testEnv(nil)
	if code := run(context.Background(), []string{"backfill", "-h"}, e); code != 0 {
		t.Errorf("help exit %d", code)
	}
}

func TestParseBackfill(t *testing.T) {
	bad := []struct{ tokens, from, to, derive string }{
		{"", "2026-09-01", "", ""},
		{"abc", "2026-09-01", "", ""},
		{"0", "2026-09-01", "", ""},
		{"1", "", "", ""},
		{"1", "2026/09/01", "", ""},
		{"1", "2026-09-02", "2026-09-01", ""},
		{"1", "2026-09-01", "", "90s"},
		{"1", "2026-09-01", "", "1m"},
		{"1", "2026-09-01", "", "x"},
		{"1", "2026-09-01", "", "7m"},
		{"1", "2026-09-01", "", "45m"},
		{"1", "2026-09-01", "", "5m,5m"},
	}
	for _, tc := range bad {
		if _, err := parseBackfill(tc.tokens, "", tc.from, tc.to, tc.derive, "data"); err == nil {
			t.Errorf("parseBackfill(%+v) accepted", tc)
		}
	}
	job, err := parseBackfill(" 5, 3,5 ", "", "2026-09-01", "", "5m,15m", "data")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(job.tokens) != "[5 3]" || !job.to.Equal(job.from) || len(job.derived) != 2 {
		t.Errorf("job = %+v", job)
	}
}

// flakyKite answers with a rate-limit error the first n times it is asked, as
// Kite does when a long study meets its limit, then serves the candles.
type flakyKite struct {
	fails, calls int
	err          error
}

func (f *flakyKite) GetHistoricalData(int, string, time.Time, time.Time, bool, bool) ([]kiteconnect.HistoricalData, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, f.err
	}
	return []kiteconnect.HistoricalData{{Date: models.Time{Time: time.Date(2026, 9, 1, 9, 15, 0, 0, ist)}, Open: 10, High: 11, Low: 9, Close: 10, Volume: 100}}, nil
}

func TestBackfillRetriesATransientKiteError(t *testing.T) {
	screen.RetryBackoff = time.Millisecond
	t.Cleanup(func() { screen.RetryBackoff = time.Second })
	job, err := parseBackfill("1", "", "2026-09-01", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := &flakyKite{fails: 2, err: kiteconnect.NewError(kiteconnect.GeneralError, "Too many requests", nil)}
	got, err := job.fetchChunk(ctx, k, rate.NewLimiter(rate.Inf, 1), job.from, job.from.Add(24*time.Hour))
	if err != nil || len(got) != 1 || k.calls != 3 {
		t.Errorf("a rate limit twice: %v, %d bars, %d calls", err, len(got), k.calls)
	}
	// An expired session is not worth another try.
	k = &flakyKite{fails: 9, err: kiteconnect.NewError(kiteconnect.TokenError, "session expired", nil)}
	if _, err := job.fetchChunk(ctx, k, rate.NewLimiter(rate.Inf, 1), job.from, job.from.Add(24*time.Hour)); err == nil || k.calls != 1 {
		t.Errorf("a token error: %v after %d calls", err, k.calls)
	}
}
