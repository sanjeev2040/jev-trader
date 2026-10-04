package study

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

func stockDay(key string, picked bool, ret float64, newsSignal float64, items int) StockDay {
	s := StockDay{Key: key, Picked: picked, ReturnPct: ret, NewsItems: items, Signals: map[string]float64{}}
	if items > 0 || newsSignal != 0 {
		s.Signals["news"] = newsSignal
	}
	day := ret * 2
	s.DayReturnPct = &day
	return s
}

// marketDays is n trading days from Monday 2026-10-05 (weekends skipped).
func marketDays(n int) []string {
	var out []string
	for d := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); len(out) < n; d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, d.Format(time.DateOnly))
		}
	}
	return out
}

// A day on which the market moved m, the random list sat on it, and the two
// picks beat it by 1.0 and 0.5 points: the excess is 0.75 whatever the market did.
func pickDay(date string, m float64) DayResult {
	d := DayResult{Version: ResultVersion, Date: date, Status: StatusOK}
	d.Stocks = []StockDay{stockDay("NSE:P1", true, m+1.0, 0.6, 1), stockDay("NSE:P2", true, m+0.5, 0.4, 1)}
	for i := range 4 {
		d.Stocks = append(d.Stocks, stockDay(fmt.Sprintf("NSE:R%d", i), false, m, 0, 0))
	}
	return d
}

func TestNeutralRemovesTheMarketsDay(t *testing.T) {
	var days []DayResult
	for i, date := range marketDays(7) { // two calendar weeks
		days = append(days, pickDay(date, []float64{-2, -1, 0, 1, 2, 0.5, -0.5}[i]))
	}
	n := Summarize("2026-10-05", "2026-10-13", days).Neutral
	if n.BenchmarkDays[BenchmarkRandom] != 7 || n.ExcludedDays != 0 || n.Universe {
		t.Fatalf("benchmark days %v, excluded %d, universe %v", n.BenchmarkDays, n.ExcludedDays, n.Universe)
	}
	picks, random := n.Lists[0], n.Lists[1]
	if picks.Name != "picks" || random.Name != "random list" {
		t.Fatalf("lists = %+v", n.Lists)
	}
	w := picks.Window
	if w.N != 14 || w.Days != 7 || !near(w.Mean, 0.75) || !near(w.MeanRaw, 0.75+(-2-1+0+1+2+0.5-0.5)/7.0) {
		t.Errorf("picks over the window = %+v", w)
	}
	// Every day beat the market by the same amount, so every resample says so.
	if !w.CI.OK || !near(w.CI.Lo, 0.75) || !near(w.CI.Hi, 0.75) || w.Weeks != 2 || w.PositiveWeeks != 2 {
		t.Errorf("picks interval %+v, weeks %d/%d", w.CI, w.PositiveWeeks, w.Weeks)
	}
	// The day-close return is twice the window's here, and so is its excess.
	if d := picks.Day; !near(d.Mean, 1.5) || d.N != 14 {
		t.Errorf("picks to the close = %+v", d)
	}
	// The random list is the benchmark: no excess at all.
	if !near(random.Window.Mean, 0) || random.Window.N != 28 || random.Window.PositiveWeeks != 0 {
		t.Errorf("random list = %+v", random.Window)
	}
	// Weeks: Monday 10-05 (5 days) and Monday 10-12 (2 days).
	if len(n.Weeks) != 2 || n.Weeks[0].Week != "2026-10-05" || n.Weeks[1].Week != "2026-10-12" || n.Weeks[0].Days != 5 || n.Weeks[1].Days != 2 {
		t.Fatalf("weeks = %+v", n.Weeks)
	}
	if c := n.Weeks[0].Picks; c.N != 10 || !near(c.Mean, 0.75) {
		t.Errorf("first week picks = %+v", c)
	}
	if c := n.Weeks[0].Positive; c.N != 10 || !near(c.Mean, 0.75) {
		t.Errorf("first week positive news = %+v", c) // both picks have positive news
	}
	if n.Weeks[0].Negative.N != 0 {
		t.Errorf("negative news in a week without any: %+v", n.Weeks[0].Negative)
	}
}

func TestBootstrapIntervalIsReproducibleAndHonest(t *testing.T) {
	// Daily excess of one stock: 1, -1, 2, 0, 1, -0.5, 1.5, 0.5.
	vals := []float64{1, -1, 2, 0, 1, -0.5, 1.5, 0.5}
	var aggs []dayAgg
	for i, v := range vals {
		aggs = append(aggs, dayAgg{day: marketDays(8)[i], sum: v, raw: v, n: 1})
	}
	m := measure(aggs)
	again := measure(aggs)
	if m.CI != again.CI {
		t.Errorf("the interval is not reproducible: %+v vs %+v", m.CI, again.CI)
	}
	if !m.CI.OK || !near(m.Mean, 0.5625) || !(m.CI.Lo < m.Mean && m.Mean < m.CI.Hi) {
		t.Errorf("mean %v interval %+v", m.Mean, m.CI)
	}
	// Eight days of this spread cannot rule out zero.
	if m.CI.Lo > 0 || m.CI.Hi < 0.5 {
		t.Errorf("interval %+v is implausibly narrow for 8 noisy days", m.CI)
	}
	// Too few days for an interval.
	if got := measure(aggs[:2]); got.CI.OK {
		t.Errorf("two days gave an interval: %+v", got.CI)
	}
	if got := measure(nil); got.N != 0 || got.CI.OK {
		t.Errorf("no days = %+v", got)
	}
	if iv := meanCI([]float64{0.1, 0.2, 0.3, 0.2}); !iv.OK || iv.Lo <= 0.1-1e-9 || iv.Hi >= 0.3+1e-9 {
		t.Errorf("meanCI = %+v", iv)
	}
}

// poolDay is a day on which the whole screened pool was measured: four stocks
// with good news up one point on the pool, four with bad news down one, twelve
// without news flat; the market moved m.
func poolDay(date string, m float64) DayResult {
	d := DayResult{Version: ResultVersion, Date: date, Status: StatusOK}
	d.Stocks = []StockDay{stockDay("NSE:P1", true, m+1, 0.7, 1)}
	for i := range 3 {
		d.Pool = append(d.Pool, stockDay(fmt.Sprintf("NSE:U%d", i), false, m+1, 0.5, 2))
	}
	for i := range 4 {
		d.Pool = append(d.Pool, stockDay(fmt.Sprintf("NSE:D%d", i), false, m-1, -0.6, 1))
	}
	for i := range 11 {
		d.Pool = append(d.Pool, stockDay(fmt.Sprintf("NSE:N%d", i), false, m, 0, 0))
	}
	d.Pool = append(d.Pool, stockDay("NSE:Z", false, m, 0, 2)) // items that cancelled
	return d
}

func TestNeutralNewsTableCoversTheWholePoolIncludingNegativeNews(t *testing.T) {
	var days []DayResult
	for i, date := range marketDays(4) {
		days = append(days, poolDay(date, float64(i)-1.5))
	}
	s := Summarize("2026-10-05", "2026-10-08", days)
	n := s.Neutral
	if !n.Universe || n.BenchmarkDays[BenchmarkPool] != 4 {
		t.Fatalf("universe %v, benchmark days %v", n.Universe, n.BenchmarkDays)
	}
	rows := map[string]NeutralRow{}
	for _, r := range n.News {
		rows[r.Name] = r
	}
	// 20 stocks a day: the pool mean is m + (4*1 - 4*1)/20 = m, so the excess is +1, -1, 0.
	if above := rows[newsAbove]; above.Window.N != 16 || !near(above.Window.Mean, 1) || !above.Window.CI.OK || !near(above.Window.CI.Lo, 1) {
		t.Errorf("news above 0 = %+v", above.Window)
	}
	if below := rows[newsBelow]; below.Window.N != 16 || !near(below.Window.Mean, -1) || !near(below.Window.CI.Hi, -1) || below.Window.PositiveWeeks != 0 {
		t.Errorf("news below 0 = %+v", below.Window)
	}
	if zero := rows[newsZero]; zero.Window.N != 4 || !near(zero.Window.Mean, 0) {
		t.Errorf("scored, net zero = %+v", zero.Window)
	}
	if none := rows[newsNone]; none.Window.N != 4*11 || !near(none.Window.Mean, 0) {
		t.Errorf("no scored news item = %+v", none.Window)
	}
	// The spread is per day: +1 against -1.
	if sp := n.Spread; sp.Window.N != 4 || sp.Window.Days != 4 || !near(sp.Window.Mean, 2) || !sp.Window.CI.OK {
		t.Errorf("spread = %+v", sp.Window)
	}
	if !near(n.Spread.Day.Mean, 4) {
		t.Errorf("spread to the close = %+v", n.Spread.Day)
	}
	// The picks are one stock a day, measured against the whole pool.
	if p := n.Lists[0].Window; p.N != 4 || !near(p.Mean, 1) {
		t.Errorf("picks = %+v", p)
	}
	// The signal table reads the whole pool, not just the picks and random list.
	var all SignalStat
	for _, st := range s.Signals {
		if st.Signal == "news" && st.Population == "all studied stocks" {
			all = st
		}
	}
	if all.N != 4*(1+3+4+1) || all.Above.N != 16 || all.Below.N != 16 {
		t.Errorf("news signal over the pool = %+v", all)
	}
	if all.ICDays != 4 || !all.ICCI.OK || all.MeanDailyIC <= 0 {
		t.Errorf("daily IC = %+v", all)
	}

	var md bytes.Buffer
	if err := WriteMarkdown(&md, s, Meta{Generated: time.Now(), CostPct: 0.15}); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{
		"## Market-neutral evidence", "the whole screened pool on 4 day(s)", "95% interval", "Weeks positive", "Long, excess after costs",
		"### The news signal", "Every screened stock-day, not only the picks", "| news below 0 | 16 | 4 | -1.00% | -1.00% | -1.00% to -1.00% | 0/1 |",
		"| news above 0 − news below 0 | 4 | 4 |", "### Week by week", "| 2026-10-05 | 4 |", "IC 95% interval",
		"Every stock measured (the whole screened pool, the picks and the random list included)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q\n%s", want, out)
		}
	}
}

func TestNeutralLeavesOutDaysWithoutACrossSection(t *testing.T) {
	thin := DayResult{Version: ResultVersion, Date: "2026-10-05", Status: StatusOK, Stocks: []StockDay{stockDay("NSE:P1", true, 1, 0.5, 1), stockDay("NSE:R", false, 0, 0, 0)}}
	full := pickDay("2026-10-06", 0)
	n := Summarize("2026-10-05", "2026-10-06", []DayResult{thin, full}).Neutral
	if n.ExcludedDays != 1 || n.Lists[0].Window.Days != 1 {
		t.Errorf("excluded %d, picks days %d", n.ExcludedDays, n.Lists[0].Window.Days)
	}
	// Without enough random stocks the picks and the random list together are the cross-section.
	few := DayResult{Version: ResultVersion, Date: "2026-10-07", Status: StatusOK, Stocks: []StockDay{
		stockDay("NSE:P1", true, 2, 0.5, 1), stockDay("NSE:P2", true, 1, 0.5, 1), stockDay("NSE:R", false, 0, 0, 0)}}
	n = Summarize("2026-10-07", "2026-10-07", []DayResult{few}).Neutral
	if n.BenchmarkDays[BenchmarkStudied] != 1 || !near(n.Lists[0].Window.Mean, 0.5) {
		t.Errorf("fallback benchmark: %v, picks %+v", n.BenchmarkDays, n.Lists[0].Window)
	}
	// Nothing at all still summarises and reports.
	var md bytes.Buffer
	if err := WriteMarkdown(&md, Summarize("a", "b", []DayResult{{Version: ResultVersion, Date: "2026-10-05", Status: StatusOK}}), Meta{Generated: time.Now()}); err != nil || !strings.Contains(md.String(), "No day had a cross-section") {
		t.Errorf("%v\n%s", err, md.String())
	}
}

func TestWeekOfIsTheMonday(t *testing.T) {
	for day, want := range map[string]string{"2026-10-05": "2026-10-05", "2026-10-09": "2026-10-05", "2026-10-11": "2026-10-05", "2026-10-12": "2026-10-12"} {
		if got := weekOf(day); got != want {
			t.Errorf("week of %s = %s, want %s", day, got, want)
		}
	}
}

// The whole pool is backfilled and measured, the deciders still trade only the
// picks and the random list, and a day with no picks is measured too.
func TestRunWithTheWholePool(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Universe = true
	f := &fixture{quiet: map[string]bool{"2026-10-06": true}}
	var backfilled [][]string
	var returned [][]string
	d := f.deps()
	inner := d.Backfill
	d.Backfill = func(ctx context.Context, day string, keys []string) ([]string, error) {
		backfilled = append(backfilled, slices.Clone(keys))
		return inner(ctx, day, keys)
	}
	f.missing = []string{"NSE:H"}
	d.Returns = func(_ context.Context, day string, keys []string) (map[string]Return, error) {
		returned = append(returned, slices.Clone(keys))
		out := map[string]Return{}
		for i, k := range keys {
			dc := float64(i)
			out[k] = Return{WindowPct: float64(i) / 2, DayPct: &dc}
		}
		return out, nil
	}
	days, err := Run(context.Background(), cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 || f.replays != 4 { // two ordinary days, two lists each; the quiet day replays nothing
		t.Fatalf("%d days, %d replays", len(days), f.replays)
	}
	// An ordinary day: 3 picks, 3 random from the 5 others, and the 2 left over are measured only.
	ord := days[0]
	if len(ord.Pool) == 0 || len(ord.Pool) > 2 {
		t.Fatalf("pool = %+v", ord.Pool)
	}
	if got := len(backfilled[0]); got != 8 {
		t.Errorf("the first day backfilled %d stocks, want the whole pool of 8: %v", got, backfilled[0])
	}
	for _, p := range ord.Pool {
		if slices.Contains([]string{"NSE:A", "NSE:B", "NSE:C", "NSE:H"}, p.Key) {
			t.Errorf("%s is a pick, in the random list or without candles, yet was measured as the rest of the pool", p.Key)
		}
	}
	for _, k := range returned[0] {
		if k == "NSE:H" {
			t.Error("Returns was asked for a stock with no candles")
		}
	}
	// The quiet day has no picks but its pool is measured: that is where negative news lives.
	quiet := days[1]
	if quiet.Status != StatusOK || quiet.Picks.Names != 0 || len(quiet.Pool) != 2 {
		t.Errorf("quiet day = %+v", quiet)
	}
	n := Summarize(cfg.From, cfg.To, days).Neutral
	// The two ordinary days have a pool cross-section; the quiet day's pool of two is too thin for one.
	if !n.Universe || n.BenchmarkDays[BenchmarkPool] != 2 || n.ExcludedDays != 1 {
		t.Errorf("benchmark days %v, excluded %d", n.BenchmarkDays, n.ExcludedDays)
	}

	// Without Returns it cannot run; without Universe it never calls it.
	d.Returns = nil
	cfg.Dir, cfg.Refresh = t.TempDir(), true
	if _, err := Run(context.Background(), cfg, d); err == nil || !strings.Contains(err.Error(), "returns") {
		t.Errorf("no Returns: %v", err)
	}
	d.Returns = func(context.Context, string, []string) (map[string]Return, error) {
		return nil, errors.New("disk full")
	}
	cfg.Dir = t.TempDir()
	if _, err := Run(context.Background(), cfg, d); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("a failed Returns: %v", err)
	}
	cfg.Universe, cfg.Dir = false, t.TempDir()
	d.Returns = func(context.Context, string, []string) (map[string]Return, error) {
		t.Error("Returns called without Universe")
		return nil, nil
	}
	if _, err := Run(context.Background(), cfg, d); err != nil {
		t.Fatal(err)
	}
}

func TestCachedDeciderSpacesOnlyThePaidCalls(t *testing.T) {
	store, err := OpenAnswerStore(t.TempDir() + "/a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	c := &CachedDecider{
		Inner: deciderFunc(func(context.Context, json.RawMessage) (domain.Decision, error) {
			calls++
			return domain.Decision{Action: domain.ActionHold}, nil
		}),
		Model: "m", Store: store, Limiter: rate.NewLimiter(rate.Limit(50), 1), // 20ms apart
	}
	start := time.Now()
	for i := range 4 {
		if _, err := c.Decide(context.Background(), []byte(fmt.Sprintf(`{"i":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	paid := time.Since(start)
	if paid < 55*time.Millisecond || calls != 4 {
		t.Errorf("4 paid calls took %v (%d reached Jev)", paid, calls)
	}
	start = time.Now()
	for i := range 4 { // all stored now
		if _, err := c.Decide(context.Background(), []byte(fmt.Sprintf(`{"i":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 15*time.Millisecond || calls != 4 {
		t.Errorf("4 stored answers took %v (%d reached Jev)", d, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Decide(ctx, []byte(`{"new":1}`)); err == nil {
		t.Error("a cancelled context waited for a slot")
	}
}

func TestReportFlagsDaysWhoseNewsScoringLostItems(t *testing.T) {
	d := pickDay("2026-10-05", 0)
	d.Notes = []string{"Index constituents are today's, not those of 2026-10-05.", "Jev failed on 3 of 40 news pairs; those are not in the scores."}
	s := Summarize("2026-10-05", "2026-10-05", []DayResult{d, pickDay("2026-10-06", 0)})
	if len(s.NewsGaps) != 1 || !strings.HasPrefix(s.NewsGaps[0], "2026-10-05: Jev failed on 3 of 40") {
		t.Fatalf("gaps = %v", s.NewsGaps)
	}
	var md bytes.Buffer
	if err := WriteMarkdown(&md, s, Meta{Generated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "## News scoring gaps") || !strings.Contains(md.String(), "only the ones that failed are asked") {
		t.Errorf("report does not flag the gap:\n%s", md.String())
	}
	clean := Summarize("2026-10-06", "2026-10-06", []DayResult{pickDay("2026-10-06", 0)})
	md.Reset()
	if err := WriteMarkdown(&md, clean, Meta{Generated: time.Now()}); err != nil || strings.Contains(md.String(), "News scoring gaps") {
		t.Errorf("a clean study mentions gaps: %v", err)
	}
}
