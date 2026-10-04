package screen

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func TestAnonymizeTextHidesTheCompanyAndTheDates(t *testing.T) {
	for _, tc := range []struct{ name, symbol, in, want string }{
		{"KPIT Technologies Ltd.", "KPITTECH", "KPIT Technologies has bagged an order worth Rs 400 crore", "the company has bagged an order worth Rs 400 crore"},
		{"KPIT Technologies Ltd.", "KPITTECH", "KPIT bags order; KPITTECH up", "the company bags order; the company up"},
		{"KPIT Technologies Ltd.", "KPITTECH", "kpit technologies, KPIT Technologies Limited report", "the company, the company Limited report"},
		{"Central Depository Services (India) Ltd.", "CDSL", "CDSL: results for the quarter ended 30 September 2026", "the company: results for the quarter ended [date]"},
		{"Mahindra & Mahindra Ltd.", "M&M", "Mahindra & Mahindra and M&M sign a deal", "the company and the company sign a deal"},
		{"Birlasoft Ltd.", "BSOFT", "Birlasoft Q2 FY27 profit rises on 2026-10-04", "the company [quarter] [period] profit rises on [date]"},
		{"Birlasoft Ltd.", "BSOFT", "Board meeting on October 12, 2026 and Monday 13/10/2026", "Board meeting on [date] and [day] [date]"},
		{"Birlasoft Ltd.", "BSOFT", "Outcome of the meeting held in March 2026", "Outcome of the meeting held in [date]"},
		// Amounts that look like years are not dates.
		{"Birlasoft Ltd.", "BSOFT", "order worth Rs 2000 crore and 2020 MW, up 2025 bps", "order worth Rs 2000 crore and 2020 MW, up 2025 bps"},
		// Another company's name is left alone.
		{"Birlasoft Ltd.", "BSOFT", "Birlasoft wins deal against Mphasis", "the company wins deal against Mphasis"},
	} {
		if got := anonymizeText(tc.in, tc.name, tc.symbol); got != tc.want {
			t.Errorf("anonymize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnonymizedRequestKeepsTheWordingAndDropsTheIdentity(t *testing.T) {
	p := pair{
		item: news.Item{Source: "nse-announcements", ID: "1", Category: "Award of Order / Receipt of Order", Title: "KPIT Technologies bags an order on 4 October 2026",
			Summary: "KPITTECH has received an order. Filed Friday.", Published: time.Date(2026, 10, 4, 18, 15, 0, 0, watchlist.IST)},
		stock: universe.Stock{Symbol: "KPITTECH", Name: "KPIT Technologies Ltd.", Industry: "Information Technology"},
	}
	named, _ := json.Marshal(p.request(false))
	anon, _ := json.Marshal(p.request(true))
	for _, leak := range []string{"KPIT", "2026", "Friday", "published", "18:15"} {
		if !strings.Contains(string(named), leak) && leak != "Friday" {
			t.Errorf("the named request lacks %q: %s", leak, named)
		}
		if strings.Contains(string(anon), leak) {
			t.Errorf("the anonymised request still has %q: %s", leak, anon)
		}
	}
	for _, keep := range []string{"bags an order", "Information Technology", "Award of Order", "nse-announcements", `"company":"the company"`} {
		if !strings.Contains(string(anon), keep) {
			t.Errorf("the anonymised request lost %q: %s", keep, anon)
		}
	}
}

// recordingJev is a fakeJev that keeps the states it was asked about.
type recordingJev struct {
	fakeJev
	smu    sync.Mutex
	states []string
}

func (r *recordingJev) Ask(ctx context.Context, state any, qs map[string]jev.Question) (jev.Answers, error) {
	body, _ := json.Marshal(state)
	r.smu.Lock()
	r.states = append(r.states, string(body))
	r.smu.Unlock()
	return r.fakeJev.Ask(ctx, state, qs)
}

func TestAnonymizedScreenScoresTheWordingWithoutTheIdentity(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	cfg.KeepPool, cfg.AnonymizeNews = true, true
	items := []news.Source{fakeNews{name: "nse-announcements", items: []news.Item{
		newsAt(18, 15, 4, "nse-announcements", "1", "KPITTECH", "KPIT Technologies has bagged an order worth Rs 400 crore on 4 October 2026"),
		newsAt(21, 0, 4, "nse-announcements", "2", "CDSL", "CDSL: Chief Financial Officer resigns w.e.f. 5 October 2026"),
	}}}
	j := &recordingJev{}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: items, Jev: j, Model: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.states) != 2 {
		t.Fatalf("%d requests: %v", len(j.states), j.states)
	}
	for _, st := range j.states {
		for _, leak := range []string{"KPIT", "CDSL", "Central Depository", "2026", "published"} {
			if strings.Contains(st, leak) {
				t.Errorf("request leaks %q: %s", leak, st)
			}
		}
	}
	// The wording still scores: an order win up, a resignation down.
	pool := map[string]watchlist.PoolEntry{}
	for _, p := range f.Pool {
		pool[p.Key] = p
	}
	if got := pool["NSE:KPITTECH"]; got.Signals[SignalNews] <= 0 || got.NewsItems != 1 {
		t.Errorf("KPIT pool entry = %+v", got)
	}
	if got := pool["NSE:CDSL"]; got.Signals[SignalNews] >= 0 || got.NewsItems != 1 {
		t.Errorf("CDSL pool entry = %+v", got)
	}
	if got := pool["NSE:BSOFT"]; got.NewsItems != 0 {
		t.Errorf("a stock with no news has %d items", got.NewsItems)
	}
	if !strings.Contains(strings.Join(f.Notes, "\n"), "anonymised") {
		t.Errorf("notes = %q", f.Notes)
	}
}

// A named and an anonymised score of the same item are different requests: one
// is never taken for the other, and each is asked once.
func TestScoreCacheKeepsNamedAndAnonymisedAnswersApart(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	deps := func(j Asker) Deps {
		return Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: testNews(), Jev: j, Model: "jev-1.13.0"}
	}
	run := func(anon bool) int {
		j := &fakeJev{}
		c := cfg
		c.AnonymizeNews = anon
		if _, err := Run(context.Background(), c, deps(j)); err != nil {
			t.Fatal(err)
		}
		return j.calls
	}
	if n := run(false); n != 4 {
		t.Fatalf("named: %d calls, want 4", n)
	}
	if n := run(false); n != 0 {
		t.Errorf("named again: %d calls, want 0", n)
	}
	if n := run(true); n != 4 {
		t.Errorf("anonymised after named: %d calls, want 4 (a different request)", n)
	}
	if n := run(true); n != 0 {
		t.Errorf("anonymised again: %d calls, want 0", n)
	}
	if n := run(false); n != 0 {
		t.Errorf("named after anonymised: %d calls, want 0 (still stored)", n)
	}
}

// The same wording about two companies is one request once the company is hidden.
func TestAnonymisedDuplicatesAreAskedOnce(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	cfg.AnonymizeNews = true
	items := []news.Source{fakeNews{name: "nse-announcements", items: []news.Item{
		newsAt(18, 15, 4, "nse-announcements", "1", "KPITTECH", "KPIT Technologies has bagged an order"),
		newsAt(18, 20, 4, "nse-announcements", "2", "BSOFT", "Birlasoft has bagged an order"),
	}}}
	j := &fakeJev{}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: items, Jev: j, Model: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if j.calls != 1 {
		t.Errorf("%d calls for one anonymised wording", j.calls)
	}
	if got := keysOf(f); !strings.Contains(got, "KPITTECH") || !strings.Contains(got, "BSOFT") {
		t.Errorf("both should be listed on the one score: %s", got)
	}
}
