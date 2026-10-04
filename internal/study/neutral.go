package study

import (
	"math/rand/v2"
	"slices"
	"sort"
	"time"
)

// Market-neutral evidence. A stock's raw return over the window is mostly the
// market's day: on a day the market rises, most names rise, whatever picked
// them. The excess return is the stock's return minus the cross-section's mean
// at the same window (the same minutes), and is what a picker can claim. Its
// uncertainty is measured by resampling whole days, because the stocks of one
// day share the market and are not independent observations.

const (
	// minBenchmark is how many stocks a day's cross-section needs to be a
	// benchmark; a day with fewer is left out of the market-neutral figures.
	minBenchmark = 3
	// bootstrapDraws is how many resamples of the days a confidence interval
	// uses. The resampling is seeded, so a report is reproducible.
	bootstrapDraws = 2000
	// minCIDays is how many days an interval needs to mean anything.
	minCIDays = 3
)

// Benchmark kinds: what a day's cross-section was.
const (
	BenchmarkPool    = "the whole screened pool"
	BenchmarkRandom  = "the random list"
	BenchmarkStudied = "the picks and the random list"
)

// Interval is a 95% confidence interval; OK is false when there were too few
// days for one.
type Interval struct {
	Lo, Hi float64
	OK     bool
}

// Measure is the excess return of a group of stock-days.
type Measure struct {
	// N is the stock-days, Days the days they fall on.
	N, Days int
	// MeanRaw is the mean return, Mean the mean excess over the cross-section,
	// both in percent and pooled over stock-days.
	MeanRaw, Mean float64
	// CI is the 95% interval of Mean, resampling days.
	CI Interval
	// Weeks counts the calendar weeks with data, PositiveWeeks those whose
	// mean excess was above zero.
	Weeks, PositiveWeeks int
}

// NeutralRow is a group of stocks measured over the window and to the close.
type NeutralRow struct {
	Name   string
	Window Measure
	Day    Measure
}

// WeekCell is a group's stock-days in a week and their mean window excess.
type WeekCell struct {
	N    int
	Mean float64
}

// WeekRow is one calendar week, named by its Monday.
type WeekRow struct {
	Week     string
	Days     int
	Picks    WeekCell
	Positive WeekCell // stocks whose news signal was above zero
	Negative WeekCell // stocks whose news signal was below zero
}

// Neutral is the study's market-neutral evidence.
type Neutral struct {
	// BenchmarkDays counts the days each kind of cross-section served;
	// ExcludedDays are the days with too few stocks for one.
	BenchmarkDays map[string]int
	ExcludedDays  int
	// Lists compares the picks and the random list to the cross-section.
	Lists []NeutralRow
	// News is the news signal over every measured stock-day: the whole
	// screened pool when the study covered it, else the picks and the random
	// list. Spread is the day-by-day difference between the stocks with news
	// above zero and below zero, over the days that had both.
	News   []NeutralRow
	Spread NeutralRow
	// Universe is true when the news table covers the whole screened pool.
	Universe bool
	Weeks    []WeekRow
}

// News signal buckets.
const (
	newsAbove = "news above 0"
	newsBelow = "news below 0"
	newsZero  = "scored, net zero"
	newsNone  = "no scored news item"
)

var newsBuckets = []string{newsAbove, newsBelow, newsZero, newsNone}

func newsBucket(s StockDay) string {
	v := s.Signals["news"]
	switch {
	case v > 0:
		return newsAbove
	case v < 0:
		return newsBelow
	case s.NewsItems > 0:
		return newsZero
	}
	return newsNone
}

// obs is one stock-day with its excess returns.
type obs struct {
	day            string
	stock          StockDay
	excess         float64
	hasDay         bool
	excessDay      float64
	random, picked bool
}

// dayStocks are the stocks measured on a day: the studied ones and the rest of
// the pool.
func dayStocks(d DayResult) []StockDay { return append(slices.Clone(d.Stocks), d.Pool...) }

// observations computes each stock-day's excess over its day's cross-section.
func observations(days []DayResult) (out []obs, kinds map[string]int, excluded int, universe bool) {
	kinds = map[string]int{}
	for _, d := range days {
		all := dayStocks(d)
		var bench []StockDay
		var kind string
		switch random := slices.DeleteFunc(slices.Clone(d.Stocks), func(s StockDay) bool { return s.Picked }); {
		case len(d.Pool) > 0:
			bench, kind, universe = all, BenchmarkPool, true
		case len(random) >= minBenchmark:
			bench, kind = random, BenchmarkRandom
		default:
			bench, kind = all, BenchmarkStudied
		}
		if len(bench) < minBenchmark {
			if len(all) > 0 {
				excluded++
			}
			continue
		}
		kinds[kind]++
		var w, dc []float64
		for _, s := range bench {
			w = append(w, s.ReturnPct)
			if s.DayReturnPct != nil {
				dc = append(dc, *s.DayReturnPct)
			}
		}
		bw, bd := mean(w), mean(dc)
		for i, s := range all {
			o := obs{day: d.Date, stock: s, excess: s.ReturnPct - bw, picked: s.Picked}
			o.random = !s.Picked && i < len(d.Stocks) // the rest of the pool is neither picked nor drawn
			if s.DayReturnPct != nil && len(dc) >= minBenchmark {
				o.hasDay, o.excessDay = true, *s.DayReturnPct-bd
			}
			out = append(out, o)
		}
	}
	return out, kinds, excluded, universe
}

// dayAgg is a group's stock-days on one day, summed.
type dayAgg struct {
	day      string
	sum, raw float64
	n        int
}

// aggregate sums the observations by day, with the value read by get.
func aggregate(os []obs, get func(obs) (raw, excess float64, ok bool)) []dayAgg {
	by := map[string]*dayAgg{}
	for _, o := range os {
		raw, ex, ok := get(o)
		if !ok {
			continue
		}
		a := by[o.day]
		if a == nil {
			a = &dayAgg{day: o.day}
			by[o.day] = a
		}
		a.sum += ex
		a.raw += raw
		a.n++
	}
	out := make([]dayAgg, 0, len(by))
	for _, a := range by {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b dayAgg) int {
		switch {
		case a.day < b.day:
			return -1
		case a.day > b.day:
			return 1
		}
		return 0
	})
	return out
}

func windowOf(o obs) (float64, float64, bool) { return o.stock.ReturnPct, o.excess, true }

func dayOf(o obs) (float64, float64, bool) {
	if !o.hasDay {
		return 0, 0, false
	}
	return *o.stock.DayReturnPct, o.excessDay, true
}

// weekOf is the Monday of the calendar week of an IST day.
func weekOf(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)).Format(time.DateOnly)
}

// measure summarises the day sums.
func measure(aggs []dayAgg) Measure {
	var m Measure
	var sum, raw float64
	weeks := map[string]*dayAgg{}
	for _, a := range aggs {
		m.N += a.n
		m.Days++
		sum += a.sum
		raw += a.raw
		w := weeks[weekOf(a.day)]
		if w == nil {
			w = &dayAgg{}
			weeks[weekOf(a.day)] = w
		}
		w.sum += a.sum
		w.n += a.n
	}
	if m.N == 0 {
		return m
	}
	m.Mean, m.MeanRaw = sum/float64(m.N), raw/float64(m.N)
	m.CI = bootstrapCI(aggs)
	m.Weeks = len(weeks)
	for _, w := range weeks {
		if w.n > 0 && w.sum > 0 {
			m.PositiveWeeks++
		}
	}
	return m
}

// bootstrapCI is the 95% percentile interval of the pooled mean, resampling
// the days with replacement. The generator is seeded, so the same days give
// the same interval.
func bootstrapCI(aggs []dayAgg) Interval {
	if len(aggs) < minCIDays {
		return Interval{}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	means := make([]float64, 0, bootstrapDraws)
	for range bootstrapDraws {
		var sum float64
		var n int
		for range aggs {
			a := aggs[rng.IntN(len(aggs))]
			sum += a.sum
			n += a.n
		}
		if n > 0 {
			means = append(means, sum/float64(n))
		}
	}
	if len(means) == 0 {
		return Interval{}
	}
	sort.Float64s(means)
	lo := means[int(0.025*float64(len(means)))]
	hi := means[min(len(means)-1, int(0.975*float64(len(means))))]
	return Interval{Lo: lo, Hi: hi, OK: true}
}

// meanCI is the 95% interval of the mean of values, one per day, resampling
// the days.
func meanCI(values []float64) Interval {
	aggs := make([]dayAgg, len(values))
	for i, v := range values {
		aggs[i] = dayAgg{sum: v, n: 1}
	}
	return bootstrapCI(aggs)
}

func summarizeNeutral(days []DayResult) Neutral {
	os, kinds, excluded, universe := observations(days)
	n := Neutral{BenchmarkDays: kinds, ExcludedDays: excluded, Universe: universe}
	row := func(name string, keep func(obs) bool) NeutralRow {
		var sel []obs
		for _, o := range os {
			if keep(o) {
				sel = append(sel, o)
			}
		}
		return NeutralRow{Name: name, Window: measure(aggregate(sel, windowOf)), Day: measure(aggregate(sel, dayOf))}
	}
	n.Lists = append(n.Lists, row("picks", func(o obs) bool { return o.picked }))
	if r := row("random list", func(o obs) bool { return o.random }); r.Window.N > 0 {
		n.Lists = append(n.Lists, r)
	}
	for _, b := range newsBuckets {
		n.News = append(n.News, row(b, func(o obs) bool { return newsBucket(o.stock) == b }))
	}

	// Above minus below, day by day.
	spread := func(get func(obs) (float64, float64, bool)) []dayAgg {
		above := aggregate(os, func(o obs) (float64, float64, bool) {
			if newsBucket(o.stock) != newsAbove {
				return 0, 0, false
			}
			return get(o)
		})
		below := aggregate(os, func(o obs) (float64, float64, bool) {
			if newsBucket(o.stock) != newsBelow {
				return 0, 0, false
			}
			return get(o)
		})
		byDay := map[string]dayAgg{}
		for _, b := range below {
			byDay[b.day] = b
		}
		var out []dayAgg
		for _, a := range above {
			if b, ok := byDay[a.day]; ok {
				out = append(out, dayAgg{day: a.day, n: 1, sum: a.sum/float64(a.n) - b.sum/float64(b.n), raw: a.raw/float64(a.n) - b.raw/float64(b.n)})
			}
		}
		return out
	}
	n.Spread = NeutralRow{Name: newsAbove + " − " + newsBelow, Window: measure(spread(windowOf)), Day: measure(spread(dayOf))}

	// Per week.
	weeks := map[string]*WeekRow{}
	sums := map[string]map[string]float64{}
	for _, o := range os {
		w := weekOf(o.day)
		r := weeks[w]
		if r == nil {
			r = &WeekRow{Week: w}
			weeks[w] = r
			sums[w] = map[string]float64{}
		}
		add := func(c *WeekCell, key string) {
			c.N++
			sums[w][key] += o.excess
		}
		if o.picked {
			add(&r.Picks, "picks")
		}
		switch newsBucket(o.stock) {
		case newsAbove:
			add(&r.Positive, "pos")
		case newsBelow:
			add(&r.Negative, "neg")
		}
	}
	for _, d := range days {
		if d.Status == StatusOK {
			if r := weeks[weekOf(d.Date)]; r != nil {
				r.Days++
			}
		}
	}
	for w, r := range weeks {
		if r.Picks.N > 0 {
			r.Picks.Mean = sums[w]["picks"] / float64(r.Picks.N)
		}
		if r.Positive.N > 0 {
			r.Positive.Mean = sums[w]["pos"] / float64(r.Positive.N)
		}
		if r.Negative.N > 0 {
			r.Negative.Mean = sums[w]["neg"] / float64(r.Negative.N)
		}
		n.Weeks = append(n.Weeks, *r)
	}
	slices.SortFunc(n.Weeks, func(a, b WeekRow) int {
		switch {
		case a.Week < b.Week:
			return -1
		case a.Week > b.Week:
			return 1
		}
		return 0
	})
	return n
}
