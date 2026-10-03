package universe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC) // 08:30 IST

// fixtureFetcher serves testdata/<name>.<ext> and counts the fetches.
func fixtureFetcher(t *testing.T, calls *atomic.Int32) Fetcher {
	t.Helper()
	return func(_ context.Context, name string) (io.ReadCloser, error) {
		calls.Add(1)
		ext := ".csv"
		if name == "asm" || name == "gsm" {
			ext = ".json"
		}
		return os.Open("testdata/" + name + ext)
	}
}

func symbols(stocks []Stock) string {
	var s []string
	for _, st := range stocks {
		s = append(s, st.Symbol)
	}
	return strings.Join(s, ",")
}

func TestLoadBuildsMidAndSmallLessLargeAndTradeForTrade(t *testing.T) {
	var calls atomic.Int32
	u, err := Load(context.Background(), t.TempDir(), testNow, fixtureFetcher(t, &calls))
	if err != nil {
		t.Fatal(err)
	}
	// ABB is a Nifty Next 50 member, TRADEONLY is series BE, KPITTECH is in
	// both lists and counted once.
	if got, want := symbols(u.Stocks), "360ONE,AIAENG,KPITTECH,ACC,BSOFT,FLAGGED"; got != want {
		t.Errorf("stocks = %s, want %s", got, want)
	}
	if got := u.Dropped["ABB"]; got != "member of niftynext50" {
		t.Errorf("ABB dropped = %q", got)
	}
	if got := u.Dropped["TRADEONLY"]; !strings.Contains(got, "trade-for-trade") {
		t.Errorf("TRADEONLY dropped = %q", got)
	}
	if u.Stocks[2].Industry != "Information Technology" || u.Stocks[2].Name != "KPIT Technologies Ltd." || u.Stocks[2].Key() != "NSE:KPITTECH" {
		t.Errorf("KPITTECH = %+v", u.Stocks[2])
	}
}

func TestLoadCachesPerISTDay(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	fetch := fixtureFetcher(t, &calls)
	if _, err := Load(context.Background(), dir, testNow, fetch); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("first load fetched %d files, want 4", calls.Load())
	}
	// Same IST day, a later pass (09:08).
	if _, err := Load(context.Background(), dir, testNow.Add(38*time.Minute), fetch); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Errorf("same-day load fetched again: %d calls", calls.Load())
	}
	// The next IST day fetches afresh.
	if _, err := Load(context.Background(), dir, testNow.Add(24*time.Hour), fetch); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 {
		t.Errorf("next-day load made %d calls in total, want 8", calls.Load())
	}
}

func TestLoadFailsWhenAnIndexCannotBeFetched(t *testing.T) {
	fetch := func(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("HTTP 403") }
	if _, err := Load(context.Background(), t.TempDir(), testNow, fetch); err == nil {
		t.Fatal("Load succeeded without any index")
	}
}

func TestParseIndexCSVErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no symbol column": "Company Name,Industry,Series\nA,B,EQ\n",
		"empty":            "",
		"header only":      "Company Name,Industry,Symbol,Series,ISIN Code\n",
	} {
		if _, err := ParseIndexCSV(strings.NewReader(body), "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A UTF-8 byte-order mark before the header must not hide the first column.
	stocks, err := ParseIndexCSV(strings.NewReader("\xef\xbb\xbfCompany Name,Industry,Symbol,Series,ISIN Code\nA Ltd.,Power,A,EQ,X\n"), "x")
	if err != nil || len(stocks) != 1 || stocks[0].Symbol != "A" {
		t.Errorf("BOM file = %v, %v", stocks, err)
	}
}

func TestFetchSurveillanceAndExclude(t *testing.T) {
	var calls atomic.Int32
	fetch := fixtureFetcher(t, &calls)
	surv, err := FetchSurveillance(context.Background(), t.TempDir(), testNow, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if surv.Size() != 3 || surv.Reasons["FLAGGED"] != "ASM LTASM - I (13)" || !strings.HasPrefix(surv.Reasons["BSOFT"], "GSM ") || surv.Reasons["ABH"] != "ASM STASM - I (11)" {
		t.Errorf("surveillance = %v", surv.Reasons)
	}
	u, err := Load(context.Background(), t.TempDir(), testNow, fetch)
	if err != nil {
		t.Fatal(err)
	}
	u.Exclude(surv.Reasons)
	if got, want := symbols(u.Stocks), "360ONE,AIAENG,KPITTECH,ACC"; got != want {
		t.Errorf("after Exclude: %s, want %s", got, want)
	}
	if u.Dropped["FLAGGED"] != "ASM LTASM - I (13)" {
		t.Errorf("FLAGGED dropped = %q", u.Dropped["FLAGGED"])
	}
}

func TestFetchSurveillancePartialFailure(t *testing.T) {
	fetch := func(_ context.Context, name string) (io.ReadCloser, error) {
		if name == "gsm" {
			return nil, errors.New("HTTP 403")
		}
		return os.Open("testdata/asm.json")
	}
	surv, err := FetchSurveillance(context.Background(), t.TempDir(), testNow, fetch)
	if err == nil || !strings.Contains(err.Error(), "gsm") {
		t.Fatalf("err = %v, want one naming gsm", err)
	}
	if surv.Reasons["FLAGGED"] == "" {
		t.Error("the ASM list that did load was dropped")
	}
}

func TestFetchSurveillanceRejectsAnEmptyReport(t *testing.T) {
	fetch := func(_ context.Context, name string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(map[string]string{"asm": `{"longterm":{"data":[]},"shortterm":{"data":[]}}`, "gsm": `[]`}[name])), nil
	}
	if _, err := FetchSurveillance(context.Background(), t.TempDir(), testNow, fetch); err == nil {
		t.Fatal("an empty report was taken as 'nothing is under surveillance'")
	}
}

func TestHTTPFetcherSendsBrowserUserAgentAndRoutes(t *testing.T) {
	var paths, agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		agents = append(agents, r.UserAgent())
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	fetch := HTTPFetcher(srv.Client(), srv.URL+"/content/indices/", srv.URL)
	for _, name := range []string{Midcap150, "asm", "gsm"} {
		body, err := fetch(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	want := "/content/indices/ind_niftymidcap150list.csv,/api/reportASM,/api/reportGSM"
	if got := strings.Join(paths, ","); got != want {
		t.Errorf("paths = %s, want %s", got, want)
	}
	for _, a := range agents {
		if !strings.HasPrefix(a, "Mozilla/5.0") {
			t.Errorf("User-Agent = %q", a)
		}
	}
	if _, err := fetch(context.Background(), "nope"); err == nil {
		t.Error("an unknown document was fetched")
	}
	missing := HTTPFetcher(srv.Client(), srv.URL+"/missing/", srv.URL)
	if _, err := missing(context.Background(), Midcap150); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404 err = %v", err)
	}
}
