package kite

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) { return "", errors.New("login required") }

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "instruments_nse.csv"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseInstruments(t *testing.T) {
	got, err := ParseInstruments(strings.NewReader(string(readFixture(t))))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d instruments, want 4", len(got))
	}
	want := domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if instrumentKey(got[3]) != "NSE:NIFTY 50" {
		t.Errorf("index row = %+v", got[3])
	}
}

func TestParseInstrumentsColumnOrderAndErrors(t *testing.T) {
	got, err := ParseInstruments(strings.NewReader("exchange,tradingsymbol,instrument_token\nNSE,INFY,408065\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != 408065 || got[0].Symbol != "INFY" {
		t.Errorf("got %+v", got)
	}

	for name, in := range map[string]string{
		"empty":          "",
		"missing column": "tradingsymbol,exchange\nINFY,NSE\n",
		"bad token":      "instrument_token,tradingsymbol,exchange\nabc,INFY,NSE\n",
	} {
		if _, err := ParseInstruments(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestResolve(t *testing.T) {
	master, err := ParseInstruments(strings.NewReader(string(readFixture(t))))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(master, []string{"infy", "NSE:TCS", " NSE:NIFTY 50 ", "NSE:INFY", ""})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, in := range got {
		keys = append(keys, instrumentKey(in))
	}
	if want := []string{"NSE:INFY", "NSE:TCS", "NSE:NIFTY 50"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("got %v, want %v", keys, want)
	}

	_, err = Resolve(master, []string{"INFY", "NOPE", "BSE:TCS"})
	if err == nil || !strings.Contains(err.Error(), "NOPE") || !strings.Contains(err.Error(), "BSE:TCS") {
		t.Errorf("expected an error naming both missing symbols, got %v", err)
	}
	if _, err := Resolve(master, nil); err == nil {
		t.Error("expected an error for an empty watchlist")
	}
}

func TestExchanges(t *testing.T) {
	got := Exchanges([]string{"INFY", "nse:TCS", "BSE:SENSEX", "", "NSE:NIFTY 50"})
	if want := []string{"NSE", "BSE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadMasterCachesPerDay(t *testing.T) {
	dir := t.TempDir()
	fixture := readFixture(t)
	calls := 0
	fetch := func(_ context.Context, exchange string) (io.ReadCloser, error) {
		calls++
		if exchange != "NSE" {
			t.Errorf("fetched %q, want NSE", exchange)
		}
		return io.NopCloser(strings.NewReader(string(fixture))), nil
	}
	// 20:00 UTC on Oct 1 is already Oct 2 in IST.
	day1 := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	for range 2 {
		got, err := LoadMaster(context.Background(), dir, day1, []string{"NSE"}, fetch)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d instruments", len(got))
		}
	}
	if calls != 1 {
		t.Errorf("fetched %d times on one day, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "instruments", "date=2026-10-02", "NSE.csv")); err != nil {
		t.Errorf("cache file: %v", err)
	}

	if _, err := LoadMaster(context.Background(), dir, day1.Add(24*time.Hour), []string{"NSE"}, fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("fetched %d times over two days, want 2", calls)
	}
}

func TestLoadMasterFailedDownloadLeavesNoCache(t *testing.T) {
	dir := t.TempDir()
	fetch := func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(strings.NewReader("instrument_token,"), errReader{})), nil
	}
	now := time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)
	if _, err := LoadMaster(context.Background(), dir, now, []string{"NSE"}, fetch); err == nil {
		t.Fatal("expected an error")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "instruments", "date=2026-10-02"))
	if len(entries) != 0 {
		t.Errorf("left files behind: %v", entries)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHTTPMasterFetcher(t *testing.T) {
	fixture := readFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instruments/NSE" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Kite-Version") != "3" || r.Header.Get("Authorization") != "token key123:tok456" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	body, err := HTTPMasterFetcher(srv.Client(), srv.URL, "key123", staticToken("tok456"))(context.Background(), "NSE")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := ParseInstruments(body)
	if err != nil || len(got) != 4 {
		t.Fatalf("got %d instruments, err %v", len(got), err)
	}

	if _, err := HTTPMasterFetcher(srv.Client(), srv.URL, "key123", staticToken("wrong"))(context.Background(), "NSE"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected an HTTP 403 error, got %v", err)
	}
	if _, err := HTTPMasterFetcher(srv.Client(), srv.URL, "key123", failingToken{})(context.Background(), "NSE"); err == nil {
		t.Error("expected a token error")
	}
}
