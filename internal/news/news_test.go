package news

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	windowFrom = time.Date(2026, 10, 4, 15, 0, 0, 0, IST) // 18h before 09:00 on the 5th
	windowTo   = time.Date(2026, 10, 5, 9, 0, 0, 0, IST)
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseRSSKeepsTheWindowAndCleansMarkup(t *testing.T) {
	items, err := parseRSS("et-stocks", strings.NewReader(string(fixture(t, "feed.xml"))), windowFrom, windowTo)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	want := []string{
		"Birlasoft shares slump after CFO resigns",
		"KPIT Technologies bags $50 million order from European carmaker",
	}
	if !reflect.DeepEqual(titles, want) {
		t.Fatalf("titles = %q, want %q (the too-old, the too-new and the undated items must be dropped)", titles, want)
	}
	if got := items[0].Summary; got != "The company said its chief financial officer resigned." {
		t.Errorf("summary = %q (markup must be stripped)", got)
	}
	if got := items[1].Summary; got != "The IT firm said the deal runs for five years & adds to its order book." {
		t.Errorf("summary = %q (entities must be decoded)", got)
	}
	if items[0].ID != "bsoft-cfo-1" || items[0].Source != "et-stocks" {
		t.Errorf("item = %+v", items[0])
	}
	if items[1].ID != "https://example.test/kpit-order" {
		t.Errorf("an item without a guid should use its link, got %q", items[1].ID)
	}
	if !items[1].Published.Equal(time.Date(2026, 10, 4, 20, 30, 0, 0, time.UTC)) {
		t.Errorf("published = %v", items[1].Published)
	}
}

func TestParseRSSRejectsHTML(t *testing.T) {
	if _, err := parseRSS("x", strings.NewReader("<!DOCTYPE html><html><body>not a feed</body></html>"), windowFrom, windowTo); err == nil {
		t.Fatal("an HTML page was taken as a feed")
	}
}

func TestRSSFetchOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Write(fixture(t, "feed.xml"))
	}))
	defer srv.Close()
	items, err := RSS{FeedName: "f", URL: srv.URL + "/feed", Client: srv.Client()}.Fetch(context.Background(), windowFrom, windowTo)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d, err = %v", len(items), err)
	}
	if _, err := (RSS{FeedName: "f", URL: srv.URL + "/gone", Client: srv.Client()}).Fetch(context.Background(), windowFrom, windowTo); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("403 err = %v", err)
	}
}

func TestNSEAnnouncementsFilterWindowAndNoise(t *testing.T) {
	var query string
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, agent = r.URL.RawQuery, r.UserAgent()
		w.Write(fixture(t, "announcements.json"))
	}))
	defer srv.Close()
	items, err := NSEAnnouncements{BaseURL: srv.URL, Client: srv.Client()}.Fetch(context.Background(), windowFrom, windowTo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index=equities", "from_date=04-10-2026", "to_date=05-10-2026"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %s", query, want)
		}
	}
	if !strings.HasPrefix(agent, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q", agent)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Symbol+"|"+it.Category)
	}
	want := []string{"KPITTECH|Bagging/Receiving of orders/contracts", "BSOFT|Resignation of Director/KMP/SMP", "ACC|Updates"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q (old, new, noise-category and symbol-less rows must go)", got, want)
	}
	if items[2].Title != "Updates" {
		t.Errorf("a row without text should fall back to its category, got %q", items[2].Title)
	}
	if !items[0].Published.Equal(time.Date(2026, 10, 4, 18, 15, 3, 0, IST)) {
		t.Errorf("published = %v", items[0].Published)
	}
}

func TestNSEAnnouncementsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("index") == "equities" {
			io.WriteString(w, `{"error":"blocked"}`)
		}
	}))
	defer srv.Close()
	if _, err := (NSEAnnouncements{BaseURL: srv.URL, Client: srv.Client()}).Fetch(context.Background(), windowFrom, windowTo); err == nil {
		t.Fatal("a non-array response was accepted")
	}
}

type fakeSource struct {
	name  string
	items []Item
	err   error
}

func (f fakeSource) Name() string { return f.name }
func (f fakeSource) Fetch(context.Context, time.Time, time.Time) ([]Item, error) {
	return f.items, f.err
}

func TestFetchAllToleratesAFailingSource(t *testing.T) {
	res := FetchAll(context.Background(), []Source{
		fakeSource{name: "a", err: errors.New("down")},
		fakeSource{name: "b", items: []Item{{Source: "b", ID: "1", Title: "x"}}},
	}, windowFrom, windowTo)
	if len(res) != 2 || res[0].Err == nil || res[1].Err != nil || len(res[1].Items) != 1 {
		t.Errorf("results = %+v", res)
	}
}

func TestDedupe(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 4, h, 0, 0, 0, IST) }
	got := Dedupe([]Item{
		{Source: "a", ID: "1", Title: "Birlasoft: CFO resigns!", Published: at(16)},
		{Source: "b", ID: "9", Title: "Birlasoft CFO resigns", Published: at(17)}, // same wire story elsewhere
		{Source: "a", ID: "1", Title: "Birlasoft: CFO resigns!", Published: at(16)},
		{Source: "n", ID: "5", Symbol: "ACC", Title: "Updates", Published: at(18)},
		{Source: "n", ID: "6", Symbol: "BSOFT", Title: "Updates", Published: at(15)}, // titles of filings are not compared
	})
	var ids []string
	for _, it := range got {
		ids = append(ids, it.Source+it.ID)
	}
	if want := []string{"n5", "a1", "n6"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v (newest first, duplicates removed)", ids, want)
	}
}

func TestMatcher(t *testing.T) {
	m := NewMatcher([]Company{
		{"KPITTECH", "KPIT Technologies Ltd."},
		{"BSOFT", "Birlasoft Ltd."},
		{"AIAENG", "AIA Engineering Ltd."},
		{"ACC", "ACC Ltd."},
		{"TATATECH", "Tata Technologies Ltd."},
		{"TATACHEM", "Tata Chemicals Ltd."},
		{"JINDALSAW", "Jindal Saw Ltd."},
		{"JINDALSTEL", "Jindal Steel & Power Ltd."},
		{"SUNPHARMA", "Sun Pharmaceutical Industries Ltd."},
		{"M&MFIN", "Mahindra & Mahindra Financial Services Ltd."},
		{"360ONE", "360 ONE WAM Ltd."},
		{"CENTURYPLY", "Century Plyboards (India) Ltd."},
		{"CENTEXT", "Century Enka Ltd."},
	})
	tests := []struct {
		name string
		it   Item
		want []string
	}{
		{"short name with the generic word dropped", Item{Title: "KPIT Q2 profit jumps as order book swells"}, []string{"KPITTECH"}},
		{"full name", Item{Title: "Tata Chemicals to expand soda ash capacity"}, []string{"TATACHEM"}},
		{"two companies", Item{Title: "Birlasoft and AIA Engineering rally"}, []string{"AIAENG", "BSOFT"}},
		{"group name alone is not a company", Item{Title: "Tata group stocks in focus"}, nil},
		{"ambiguous alias is dropped", Item{Title: "Jindal shares surge"}, nil},
		{"full name still matches when its short form is ambiguous", Item{Title: "Jindal Saw wins pipe order"}, []string{"JINDALSAW"}},
		{"too short to match by name", Item{Title: "Acc is an English word"}, nil},
		{"symbol in capitals", Item{Title: "Brokerages upgrade BSOFT"}, []string{"BSOFT"}},
		{"symbol in the summary", Item{Title: "IT midcaps rally", Summary: "KPITTECH led gains"}, []string{"KPITTECH"}},
		{"word inside another word", Item{Title: "Birlasoftware is not a company"}, nil},
		{"ampersand names", Item{Title: "Mahindra & Mahindra Financial Services posts record disbursals"}, []string{"M&MFIN"}},
		{"numeric name", Item{Title: "360 ONE WAM sees inflows"}, []string{"360ONE"}},
		{"item naming its own symbol", Item{Symbol: "ACC", Title: "Updates"}, []string{"ACC"}},
		{"item naming a symbol outside the universe", Item{Symbol: "RELIANCE", Title: "KPIT"}, nil},
		{"parenthesised suffix", Item{Title: "Century Plyboards (India) cuts guidance"}, []string{"CENTURYPLY"}},
		{"a shared first word is not enough", Item{Title: "Century stocks rise"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.Match(tt.it); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Match(%q) = %v, want %v", tt.it.Title, got, tt.want)
			}
		})
	}
}
