package screen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

const testDate = "2026-10-05" // a Monday

var (
	testDay = time.Date(2026, 10, 5, 0, 0, 0, 0, watchlist.IST)
	// 08:30 IST, before pre-open; and 09:09 IST, after it.
	earlyNow = time.Date(2026, 10, 5, 8, 30, 0, 0, watchlist.IST)
	finalNow = time.Date(2026, 10, 5, 9, 9, 0, 0, watchlist.IST)
)

// stockSpec describes one fixture stock.
type stockSpec struct {
	symbol, name, industry string
	index                  string // universe.Midcap150 or Smallcap250
	series                 string // default EQ
	price                  float64
	volume                 int64
	prevRet                float64 // previous session return, percent
	rangePct               float64
	sessions               int // default 25
	gap                    *float64
}

func gapOf(x float64) *float64 { return &x }

var fixtureStocks = []stockSpec{
	// Information Technology: strong previous session.
	{symbol: "KPITTECH", name: "KPIT Technologies Ltd.", industry: "Information Technology", index: universe.Midcap150, price: 1400, volume: 900_000, prevRet: 2.5, rangePct: 3.0, gap: gapOf(1.8)},
	{symbol: "BSOFT", name: "Birlasoft Ltd.", industry: "Information Technology", index: universe.Midcap150, price: 600, volume: 1_500_000, prevRet: 2.0, rangePct: 2.5, gap: gapOf(0.9)},
	{symbol: "MPHASIS", name: "Mphasis Ltd.", industry: "Information Technology", index: universe.Midcap150, price: 2600, volume: 400_000, prevRet: 1.5, rangePct: 2.0, gap: gapOf(0.4)},
	{symbol: "TANLA", name: "Tanla Platforms Ltd.", industry: "Information Technology", index: universe.Smallcap250, price: 800, volume: 700_000, prevRet: 1.8, rangePct: 2.2, gap: gapOf(0.6)},
	// Financial Services: weak.
	{symbol: "CDSL", name: "Central Depository Services (India) Ltd.", industry: "Financial Services", index: universe.Midcap150, price: 1500, volume: 800_000, prevRet: -1.5, rangePct: 3.5, gap: gapOf(-0.8)},
	{symbol: "ANGELONE", name: "Angel One Ltd.", industry: "Financial Services", index: universe.Midcap150, price: 2500, volume: 500_000, prevRet: -2.0, rangePct: 4.0, gap: gapOf(-1.2)},
	{symbol: "CAMS", name: "Computer Age Management Services Ltd.", industry: "Financial Services", index: universe.Smallcap250, price: 4000, volume: 300_000, prevRet: -1.0, rangePct: 2.8, gap: gapOf(-0.5)},
	// Realty: flat.
	{symbol: "OBEROIRLTY", name: "Oberoi Realty Ltd.", industry: "Realty", index: universe.Midcap150, price: 1700, volume: 600_000, prevRet: 0.2, rangePct: 2.6, gap: gapOf(0.1)},
	{symbol: "PHOENIXLTD", name: "The Phoenix Mills Ltd.", industry: "Realty", index: universe.Midcap150, price: 1600, volume: 600_000, prevRet: 0.0, rangePct: 2.4, gap: gapOf(0.0)},
	{symbol: "BRIGADE", name: "Brigade Enterprises Ltd.", industry: "Realty", index: universe.Smallcap250, price: 1100, volume: 700_000, prevRet: -0.1, rangePct: 2.7, gap: gapOf(-0.1)},
	// Never candidates.
	{symbol: "ABB", name: "ABB India Ltd.", industry: "Capital Goods", index: universe.Midcap150, price: 6000, volume: 900_000, prevRet: 3.0, rangePct: 3, gap: gapOf(2)}, // Nifty Next 50
	{symbol: "TRADEONLY", name: "Trade Only Ltd.", industry: "Capital Goods", index: universe.Smallcap250, series: "BE", price: 300, volume: 900_000, prevRet: 3.0, rangePct: 3, gap: gapOf(2)},
	{symbol: "FLAGGED", name: "Flagged Industries Ltd.", industry: "Metals & Mining", index: universe.Smallcap250, price: 300, volume: 900_000, prevRet: 3.0, rangePct: 3, gap: gapOf(2)}, // ASM
	{symbol: "ILLIQUID", name: "Illiquid Holdings Ltd.", industry: "Metals & Mining", index: universe.Smallcap250, price: 200, volume: 20_000, prevRet: 3.0, rangePct: 3, gap: gapOf(2)},
	{symbol: "PENNY", name: "Penny Works Ltd.", industry: "Metals & Mining", index: universe.Smallcap250, price: 20, volume: 9_000_000, prevRet: 3.0, rangePct: 3, gap: gapOf(2)},
	{symbol: "NEWLIST", name: "Newly Listed Ltd.", industry: "Metals & Mining", index: universe.Smallcap250, price: 500, volume: 900_000, prevRet: 3.0, rangePct: 3, sessions: 4, gap: gapOf(2)},
}

func indexCSV(index string) string {
	var b strings.Builder
	b.WriteString("Company Name,Industry,Symbol,Series,ISIN Code\n")
	for _, s := range fixtureStocks {
		if s.index != index {
			continue
		}
		series := s.series
		if series == "" {
			series = "EQ"
		}
		fmt.Fprintf(&b, "%s,%s,%s,%s,INE000000000\n", quoteCSV(s.name), quoteCSV(s.industry), s.symbol, series)
	}
	return b.String()
}

func quoteCSV(s string) string { return `"` + s + `"` }

// nseFetcher serves the fixture universe.
func nseFetcher() universe.Fetcher {
	docs := map[string]string{
		universe.Midcap150:   indexCSV(universe.Midcap150),
		universe.Smallcap250: indexCSV(universe.Smallcap250),
		universe.Nifty50:     "Company Name,Industry,Symbol,Series,ISIN Code\nInfosys Ltd.,Information Technology,INFY,EQ,X\n",
		universe.NiftyNext50: "Company Name,Industry,Symbol,Series,ISIN Code\nABB India Ltd.,Capital Goods,ABB,EQ,X\n",
		"asm":                `{"longterm":{"data":[{"symbol":"FLAGGED","survCode":"LTASM - I (13)"}]},"shortterm":{"data":[]}}`,
		"gsm":                `[{"symbol":"ZZZGSM","survCode":"GSM 0"}]`,
	}
	return func(_ context.Context, name string) (io.ReadCloser, error) {
		body, ok := docs[name]
		if !ok {
			return nil, fmt.Errorf("no fixture for %s", name)
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

// fakeMarket serves candles built from fixtureStocks and quotes with the
// specs' gaps.
type fakeMarket struct {
	mu          sync.Mutex
	dailyCalls  map[string]int
	quoteCalls  int
	dailyErr    error // returned for every Daily call
	quotesErr   error
	quotesFlat  bool // quotes repeat the previous close (before pre-open)
	indexQuotes map[string]Quote
}

func (m *fakeMarket) Daily(_ context.Context, key string, _, _ time.Time) ([]Candle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dailyCalls == nil {
		m.dailyCalls = map[string]int{}
	}
	m.dailyCalls[key]++
	if m.dailyErr != nil {
		return nil, m.dailyErr
	}
	for _, s := range fixtureStocks {
		if "NSE:"+s.symbol == key {
			return s.candles(testDate), nil
		}
	}
	return nil, fmt.Errorf("no candles for %s", key)
}

func (m *fakeMarket) Quotes(_ context.Context, keys []string) (map[string]Quote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quoteCalls++
	if m.quotesErr != nil {
		return nil, m.quotesErr
	}
	out := map[string]Quote{}
	for _, s := range fixtureStocks {
		prev := s.lastClose()
		last := prev
		if s.gap != nil && !m.quotesFlat {
			last = prev * (1 + *s.gap/100)
		}
		out["NSE:"+s.symbol] = Quote{Last: last, PrevClose: prev}
	}
	for k, q := range m.indexQuotes {
		out[k] = q
	}
	return out, nil
}

func (s stockSpec) nSessions() int {
	if s.sessions > 0 {
		return s.sessions
	}
	return 25
}

// candles returns the stock's daily candles ending the day before date, with
// the previous session's return equal to prevRet.
func (s stockSpec) candles(date string) []Candle {
	day, _ := time.Parse(time.DateOnly, date)
	n := s.nSessions()
	out := make([]Candle, n)
	last := s.price
	prev := last / (1 + s.prevRet/100)
	for i := range n {
		close := prev
		if i == n-1 {
			close = last
		}
		out[i] = Candle{
			Date: day.AddDate(0, 0, -(n - i)).Format(time.DateOnly),
			Open: close, High: close * (1 + s.rangePct/200), Low: close * (1 - s.rangePct/200), Close: close,
			Volume: s.volume,
		}
	}
	return out
}

func (s stockSpec) lastClose() float64 { return s.price }

// fakeJev answers news questions from the headline.
type fakeJev struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeJev) Ask(_ context.Context, state any, qs map[string]jev.Question) (jev.Answers, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return jev.Answers{}, f.err
	}
	if len(qs) != 3 {
		return jev.Answers{}, fmt.Errorf("asked %d questions, want 3", len(qs))
	}
	st := state.(map[string]any)
	headline := strings.ToLower(st["headline"].(string))
	var impact, material, up, down float64
	switch {
	case strings.Contains(headline, "order"):
		impact, material, up, down = 3.2, 0.9, 0.85, 0.05
	case strings.Contains(headline, "resign"):
		impact, material, up, down = 2.4, 0.8, 0.05, 0.85
	default:
		impact, material, up, down = 0.3, 0.1, 0.2, 0.2
	}
	return jev.Answers{
		Model: "jev-1.13.0", InputTokens: 300, OutputTokens: 30,
		Answers: map[string]jev.Answer{
			qImpact:    {Type: jev.TypeScore, Score: impact},
			qMaterial:  {Type: jev.TypeNoul, Noul: material},
			qDirection: {Type: jev.TypeChoice, Choice: dirUp, Probabilities: map[string]float64{dirUp: up, dirDown: down, dirNeutral: 1 - up - down}},
		},
	}, nil
}

type fakeNews struct {
	name  string
	items []news.Item
	err   error
}

func (f fakeNews) Name() string { return f.name }
func (f fakeNews) Fetch(_ context.Context, from, to time.Time) ([]news.Item, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []news.Item
	for _, it := range f.items {
		if !it.Published.Before(from) && it.Published.Before(to) {
			out = append(out, it)
		}
	}
	return out, nil
}

func newsAt(h, m int, day int, source, id, symbol, title string) news.Item {
	return news.Item{Source: source, ID: id, Symbol: symbol, Title: title, Published: time.Date(2026, 10, day, h, m, 0, 0, watchlist.IST)}
}

func testNews() []news.Source {
	return []news.Source{
		fakeNews{name: "nse-announcements", items: []news.Item{
			newsAt(18, 15, 4, "nse-announcements", "1", "KPITTECH", "KPIT Technologies has bagged an order worth Rs 400 crore"),
			newsAt(21, 0, 4, "nse-announcements", "2", "CDSL", "CDSL: Chief Financial Officer resigns"),
			newsAt(19, 0, 4, "nse-announcements", "3", "MPHASIS", "Mphasis: investor meet intimation"),
			newsAt(10, 0, 3, "nse-announcements", "4", "OBEROIRLTY", "an order that is too old to count"),
		}},
		fakeNews{name: "et-markets", items: []news.Item{
			newsAt(22, 30, 4, "et-markets", "a", "", "Birlasoft wins large order from European bank"),
		}},
	}
}

func baseConfig(t *testing.T, phase string, now time.Time) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Date, cfg.Now, cfg.Phase = testDay, now, phase
	cfg.CacheDir = t.TempDir()
	cfg.TopN, cfg.MaxPerSector = 6, 3
	cfg.SectorIndexes = map[string]string{"Information Technology": "NSE:NIFTY IT"}
	return cfg
}

func keysOf(f watchlist.File) string { return strings.Join(f.Keys(), ",") }

func source(f watchlist.File, name string) (watchlist.Source, bool) {
	for _, s := range f.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return watchlist.Source{}, false
}

func TestEarlyScreenRanksOnNewsSectorAndRange(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	market := &fakeMarket{}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}, Model: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Date != testDate || f.Phase != watchlist.PhaseEarly || f.Final || f.Model != "jev-1.13.0" {
		t.Errorf("header = %+v", f)
	}
	if market.quoteCalls != 0 {
		t.Error("the early phase asked Kite for quotes")
	}
	// Large caps, trade-for-trade, ASM, illiquid, penny and young stocks are out.
	for _, never := range []string{"ABB", "TRADEONLY", "FLAGGED", "ILLIQUID", "PENNY", "NEWLIST"} {
		if strings.Contains(keysOf(f), "NSE:"+never) {
			t.Errorf("%s is on the list: %s", never, keysOf(f))
		}
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Entries[0].Key != "NSE:KPITTECH" || f.Entries[1].Key != "NSE:BSOFT" {
		t.Errorf("top two = %s, want KPITTECH (order win, best sector) then BSOFT", keysOf(f))
	}
	// IT has four liquid members and the cap is three.
	it := 0
	for _, e := range f.Entries {
		if e.Sector == "Information Technology" {
			it++
		}
	}
	if it != 3 {
		t.Errorf("%d IT entries in %s, want 3 (per-sector cap)", it, keysOf(f))
	}
	// A stock with only bad news and a weak sector is not a long candidate.
	if strings.Contains(keysOf(f), "NSE:CDSL") {
		t.Errorf("CDSL (CFO resigns, weak sector) is on a long list: %s", keysOf(f))
	}
	top := f.Entries[0]
	if top.Signals[SignalNews] <= 0 || top.Signals[SignalSector] <= 0 || len(top.News) == 0 {
		t.Errorf("top entry signals/news = %v / %v", top.Signals, top.News)
	}
	if _, has := top.Signals[SignalGap]; has {
		t.Errorf("early phase has a gap signal: %v", top.Signals)
	}
	if top.News[0].Impact != 3.2 || top.News[0].Direction <= 0.7 {
		t.Errorf("news ref = %+v", top.News[0])
	}
	var newsReason string
	for _, r := range top.Reasons {
		if strings.HasPrefix(r, "news ") {
			newsReason = r
		}
	}
	if len(top.Reasons) < 4 || !strings.Contains(newsReason, "bagged an order") || !strings.Contains(newsReason, "impact 3.2/4") {
		t.Errorf("reasons = %q", top.Reasons)
	}
	if !strings.HasPrefix(top.Reasons[len(top.Reasons)-1], "liquidity: ") {
		t.Errorf("last reason = %q", top.Reasons[len(top.Reasons)-1])
	}
	for _, e := range f.Entries {
		if e.Rank < 1 || e.Score < cfg.MinScore {
			t.Errorf("entry %+v below the minimum score or unranked", e)
		}
	}
	for i := 1; i < len(f.Entries); i++ {
		if f.Entries[i].Score > f.Entries[i-1].Score {
			t.Errorf("not in score order: %s", keysOf(f))
		}
	}
	// The sector table puts IT first.
	if f.Sectors[0].Name != "Information Technology" || f.Sectors[0].Gap != nil {
		t.Errorf("sectors[0] = %+v", f.Sectors[0])
	}
	// KPIT, CDSL and MPHASIS filings and the Birlasoft story are in the
	// window; the Oberoi filing is a day too old.
	if s, ok := source(f, "jev"); !ok || s.Status != watchlist.StatusOK || s.Items != 4 || !strings.Contains(s.Detail, "1200 input tokens") {
		t.Errorf("jev source = %+v", s)
	}
}

func TestFinalScreenAddsThePreOpenGap(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseFinal, finalNow)
	market := &fakeMarket{indexQuotes: map[string]Quote{"NSE:NIFTY IT": {Last: 101, PrevClose: 100}}}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}, Model: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Final || f.Phase != watchlist.PhaseFinal {
		t.Errorf("phase/final = %s/%v", f.Phase, f.Final)
	}
	top := f.Entries[0]
	if g := top.Signals[SignalGap]; g <= 0.6 {
		t.Errorf("KPITTECH gap signal = %v, want a +1.8%% gap to read as about +0.7", g)
	}
	found := false
	for _, r := range top.Reasons {
		if strings.HasPrefix(r, "pre-open gap +1.8%") {
			found = true
		}
	}
	if !found {
		t.Errorf("no gap reason in %q", top.Reasons)
	}
	if it := f.Sectors[0]; it.Gap == nil || it.IndexChange == nil || *it.IndexChange < 0.99 || it.Index != "NSE:NIFTY IT" {
		t.Errorf("IT sector = %+v", it)
	}
	if q, ok := source(f, "kite-quotes"); !ok || q.Status != watchlist.StatusOK {
		t.Errorf("quotes source = %+v", q)
	}
}

func TestFinalScreenIgnoresAGapThatIsNotThereYet(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseFinal, finalNow)
	market := &fakeMarket{quotesFlat: true}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}, Model: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := f.Entries[0].Signals[SignalGap]; has {
		t.Errorf("a flat quote became a gap signal: %v", f.Entries[0].Signals)
	}
	if !strings.Contains(strings.Join(f.Notes, "\n"), "no pre-open gap to read") {
		t.Errorf("notes = %q", f.Notes)
	}
}

func TestQuotesFailureCostsOnlyTheGap(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseFinal, finalNow)
	market := &fakeMarket{quotesErr: errors.New("kite 503")}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := source(f, "kite-quotes"); s.Status != watchlist.StatusFailed {
		t.Errorf("quotes source = %+v", s)
	}
	if len(f.Entries) == 0 {
		t.Error("no entries")
	}
}

func TestHistoricRunNeverReadsQuotes(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseHistoric, time.Date(2026, 10, 7, 12, 0, 0, 0, watchlist.IST))
	market := &fakeMarket{}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}})
	if err != nil {
		t.Fatal(err)
	}
	if market.quoteCalls != 0 || !f.Final {
		t.Errorf("quoteCalls = %d, final = %v", market.quoteCalls, f.Final)
	}
	// The news window ends at 09:00 on the screened day, not at Now, so the
	// 22:30 and 21:00 evening items are inside it.
	if s, _ := source(f, "jev"); s.Items == 0 {
		t.Errorf("jev source = %+v", s)
	}
}

func TestBadNewsMakesAShortCandidateOnlyInBothMode(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	cfg.Direction = "both"
	cfg.TopN = 10
	cfg.MaxPerSector = 0
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: testNews(), Jev: &fakeJev{}})
	if err != nil {
		t.Fatal(err)
	}
	var cdsl *watchlist.Entry
	for i := range f.Entries {
		if f.Entries[i].Key == "NSE:CDSL" {
			cdsl = &f.Entries[i]
		}
	}
	if cdsl == nil {
		t.Fatalf("CDSL not in %s", keysOf(f))
	}
	if cdsl.Score >= 0 || cdsl.Reasons[0] != "short candidate: the signals point down" {
		t.Errorf("CDSL = score %v, reasons %q", cdsl.Score, cdsl.Reasons)
	}
}

func TestWithoutJevTheNewsSignalIsLeftOutNotZero(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: testNews(), Jev: nil})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := source(f, "jev"); s.Status != watchlist.StatusSkipped {
		t.Errorf("jev source = %+v", s)
	}
	if _, has := f.Entries[0].Signals[SignalNews]; has {
		t.Errorf("news signal present without Jev: %v", f.Entries[0].Signals)
	}
	if !strings.Contains(strings.Join(f.Notes, "\n"), "news signal was not available") {
		t.Errorf("notes = %q", f.Notes)
	}
}

func TestJevFailingEverywhereStillYieldsAList(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	j := &fakeJev{err: &domainErr{"jev: HTTP 401"}}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: testNews(), Jev: j})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := source(f, "jev"); s.Status != watchlist.StatusFailed || !strings.Contains(s.Detail, "401") {
		t.Errorf("jev source = %+v", s)
	}
	if j.calls > 3+failFast+cfg.NewsConcurrency {
		t.Errorf("%d Jev calls after the first %d failed; it should give up", j.calls, failFast)
	}
	if len(f.Entries) == 0 {
		t.Fatal("no entries")
	}
	if _, has := f.Entries[0].Signals[SignalNews]; has {
		t.Errorf("news signal after total Jev failure: %v", f.Entries[0].Signals)
	}
}

type domainErr struct{ s string }

func (e *domainErr) Error() string { return e.s }

func TestNewsSourcesFailingIndependently(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	srcs := []news.Source{fakeNews{name: "dead", err: errors.New("HTTP 403")}, testNews()[0]}
	f, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: srcs, Jev: &fakeJev{}})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := source(f, "dead"); s.Status != watchlist.StatusFailed {
		t.Errorf("dead source = %+v", s)
	}
	if s, _ := source(f, "nse-announcements"); s.Status != watchlist.StatusOK {
		t.Errorf("working source = %+v", s)
	}
	if _, has := f.Entries[0].Signals[SignalNews]; !has {
		t.Error("a failing feed took the news signal down with it")
	}
}

func TestCandlesAreCachedForTheSecondPass(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	market := &fakeMarket{}
	deps := Deps{NSE: nseFetcher(), Market: market, News: testNews(), Jev: &fakeJev{}}
	if _, err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	first := len(market.dailyCalls)
	if first == 0 {
		t.Fatal("no daily candle requests")
	}
	cfg.Phase, cfg.Now = watchlist.PhaseFinal, finalNow
	if _, err := Run(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	for key, n := range market.dailyCalls {
		if n != 1 {
			t.Errorf("%s fetched %d times", key, n)
		}
	}
}

func TestFailuresThatCannotProduceAList(t *testing.T) {
	t.Run("no index files", func(t *testing.T) {
		nse := func(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("HTTP 403") }
		_, err := Run(context.Background(), baseConfig(t, watchlist.PhaseEarly, earlyNow), Deps{NSE: nse, Market: &fakeMarket{}})
		if err == nil || !strings.Contains(err.Error(), "universe") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("expired Kite session", func(t *testing.T) {
		market := &fakeMarket{dailyErr: fmt.Errorf("kite TokenException: %w", ErrAbort)}
		_, err := Run(context.Background(), baseConfig(t, watchlist.PhaseEarly, earlyNow), Deps{NSE: nseFetcher(), Market: market})
		if !errors.Is(err, ErrAbort) {
			t.Errorf("err = %v", err)
		}
		if len(market.dailyCalls) != 1 {
			t.Errorf("%d stocks requested after an aborting error, want 1", len(market.dailyCalls))
		}
	})
	t.Run("most candle requests fail", func(t *testing.T) {
		market := &fakeMarket{dailyErr: errors.New("HTTP 429")}
		_, err := Run(context.Background(), baseConfig(t, watchlist.PhaseEarly, earlyNow), Deps{NSE: nseFetcher(), Market: market})
		if err == nil || !strings.Contains(err.Error(), "daily candles failed") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("nothing scores above the minimum", func(t *testing.T) {
		cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
		cfg.MinScore = 0.99
		_, err := Run(context.Background(), cfg, Deps{NSE: nseFetcher(), Market: &fakeMarket{}})
		if err == nil || !strings.Contains(err.Error(), "no stock scored above") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("surveillance lists unreadable", func(t *testing.T) {
		inner := nseFetcher()
		nse := func(ctx context.Context, name string) (io.ReadCloser, error) {
			if name == "asm" || name == "gsm" {
				return nil, errors.New("HTTP 403")
			}
			return inner(ctx, name)
		}
		f, err := Run(context.Background(), baseConfig(t, watchlist.PhaseEarly, earlyNow), Deps{NSE: nse, Market: &fakeMarket{}})
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := source(f, "nse-asm-gsm"); s.Status != watchlist.StatusFailed {
			t.Errorf("surveillance source = %+v", s)
		}
		if !strings.Contains(strings.Join(f.Notes, "\n"), "surveillance") {
			t.Errorf("notes = %q", f.Notes)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	good := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"phase":      func(c *Config) { c.Phase = "soon" },
		"top":        func(c *Config) { c.TopN = 0 },
		"direction":  func(c *Config) { c.Direction = "short" },
		"weights":    func(c *Config) { c.Weights = Weights{} },
		"negative":   func(c *Config) { c.Weights.News = -1 },
		"price band": func(c *Config) { c.Filters.MaxPrice = c.Filters.MinPrice },
		"scale":      func(c *Config) { c.GapScale = 0 },
		"cache":      func(c *Config) { c.CacheDir = "" },
	} {
		c := good
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
}

func TestPercentileSignals(t *testing.T) {
	got := percentileSignals(map[string]float64{"a": 1, "b": 2, "c": 2, "d": 9})
	want := map[string]float64{"a": -1, "b": 0, "c": 0, "d": 1}
	// b and c tie at ranks 1 and 2, so both get the mean rank 1.5 of 3.
	want["b"], want["c"] = 2*1.5/3-1, 2*1.5/3-1
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestCombineLeavesUnavailableSignalsOut(t *testing.T) {
	w := map[string]float64{SignalNews: 0.5, SignalGap: 0.5}
	score, contrib := combine(w, map[string]bool{SignalNews: true, SignalGap: false}, map[string]float64{SignalNews: 0.8, SignalGap: 1})
	if score != 0.8 || len(contrib) != 1 {
		t.Errorf("score = %v, contrib = %v: an unavailable signal must not dilute the others", score, contrib)
	}
}

func TestComputeStats(t *testing.T) {
	s := fixtureStocks[0].candles(testDate)
	st, why := computeStats(s, testDate, 10)
	if why != "" {
		t.Fatal(why)
	}
	if st.PrevReturn < 2.49 || st.PrevReturn > 2.51 || st.PrevClose != 1400 || st.Sessions != 25 {
		t.Errorf("stats = %+v", st)
	}
	wantValue := 900_000 * (1400*(1+0.015) + 1400*(1-0.015) + 1400) / 3
	if st.PrevValue < wantValue*0.999 || st.PrevValue > wantValue*1.001 {
		t.Errorf("PrevValue = %v, want about %v", st.PrevValue, wantValue)
	}
	// The day's own candle, present in a rerun after the open, is ignored.
	withToday := append(append([]Candle(nil), s...), Candle{Date: testDate, Open: 1, High: 1, Low: 1, Close: 1, Volume: 1})
	if st2, _ := computeStats(withToday, testDate, 10); st2.PrevClose != 1400 {
		t.Errorf("today's candle leaked into the stats: %+v", st2)
	}
	if _, why := computeStats(s[:5], testDate, 10); why == "" {
		t.Error("5 sessions accepted")
	}
	if _, why := computeStats(s, "2026-12-25", 10); !strings.Contains(why, "suspended") {
		t.Errorf("stale candles: %q", why)
	}
}

func TestSecondPassReusesTheEarlyNewsScores(t *testing.T) {
	cfg := baseConfig(t, watchlist.PhaseEarly, earlyNow)
	j := &fakeJev{}
	deps := Deps{NSE: nseFetcher(), Market: &fakeMarket{}, News: testNews(), Jev: j, Model: "jev-1.13.0"}
	early, err := Run(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	first := j.calls
	if first != 4 {
		t.Fatalf("early pass asked Jev %d times, want 4", first)
	}

	cfg.Phase, cfg.Now = watchlist.PhaseFinal, finalNow
	final, err := Run(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if j.calls != first {
		t.Errorf("final pass asked Jev %d more times for news it had already scored", j.calls-first)
	}
	if s, _ := source(final, "jev"); !strings.Contains(s.Detail, "0 asked now, 4 from today's earlier pass") || s.Items != 4 {
		t.Errorf("jev source = %+v", s)
	}
	// The same scores, a little older by 09:09.
	if d := early.Entries[0].Signals[SignalNews] - final.Entries[0].Signals[SignalNews]; d < 0 || d > 0.02 {
		t.Errorf("news signal went %v then %v", early.Entries[0].Signals, final.Entries[0].Signals)
	}

	// Jev being down at 09:08 does not lose the scores from 08:30.
	deps.Jev = &fakeJev{err: errors.New("HTTP 529")}
	deps.News = append(deps.News, fakeNews{name: "late", items: []news.Item{newsAt(8, 50, 5, "late", "z", "BSOFT", "Birlasoft late order win")}})
	cfg.Now = finalNow.Add(time.Minute)
	again, err := Run(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := source(again, "jev"); s.Status != watchlist.StatusOK || s.Items != 4 {
		t.Errorf("jev source = %+v", s)
	}
	if got := again.Entries[0].Signals[SignalNews]; got < early.Entries[0].Signals[SignalNews]-0.02 {
		t.Errorf("early news score lost when Jev went down: %v", again.Entries[0].Signals)
	}
}
