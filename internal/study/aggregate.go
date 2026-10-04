package study

import (
	"math"
	"slices"
	"strings"

	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/screen"
)

// jevDecider is the name the study gives the Jev decider.
const jevDecider = "jev"

// signalNames are the signals of the screen's score, in report order, then the
// score itself.
var signalNames = []string{screen.SignalSector, screen.SignalNews, screen.SignalRange, screen.SignalGap, "score"}

// minDailyNames is how many names a day needs for its rank correlation to count.
const minDailyNames = 8

// Summary aggregates the day results of a study.
type Summary struct {
	From, To string
	Days     []DayResult
	OKDays   int
	Skipped  []DayResult

	// Picks and Random are the buy-and-hold of the two lists.
	Picks  ListSummary
	Random ListSummary
	// Deciders are the picks' runs, RandomDeciders the random list's.
	Deciders       []DeciderSummary
	RandomDeciders []DeciderSummary
	// Paired are day-by-day differences between two results.
	Paired []Paired
	// Calibration is per decider over the picks, bins merged over all days.
	Calibration []CalibrationSummary
	// Calls scores each decider's BUY and SELL answers on the picks against
	// the price after, per horizon, over all days.
	Calls []CallsSummary
	// Signals relate each signal to the window return of the studied stocks.
	Signals []SignalStat
	// Neutral is the market-neutral evidence: excess returns over the day's
	// cross-section, with day-resampled intervals and per-week consistency,
	// and the news signal over every measured stock.
	Neutral Neutral

	// NewsGaps are the days whose news scoring lost items to Jev failures
	// (their scores are stored as they were; a rerun with Refresh asks again
	// for only what failed), as "date: the screen's note".
	NewsGaps []string

	Usage              Usage
	ScreenInputTokens  int
	ScreenOutputTokens int
}

// ListSummary is a list's buy-and-hold over the days.
type ListSummary struct {
	Days          int
	Net           float64
	Gross         float64
	Costs         float64
	PositiveDays  int
	Names         int // stock-days with candles
	NamesUp       int
	UpShare       float64
	MeanReturnPct float64 // mean over stock-days, before costs
}

// DeciderSummary is one decider's runs over the days.
type DeciderSummary struct {
	Decider      string
	Days         int
	NetPnL       float64
	GrossPnL     float64
	Costs        float64
	MeanPerDay   float64
	PositiveDays int
	Buys, Sells  int
	Holds        int
	// HoldSubstitutions are the decider failures that became HOLDs.
	HoldSubstitutions int
	Trades, Wins      int
	HitRate           float64 // Wins / Trades
	WorstDrawdown     float64
	Turnover          float64
}

// Paired is a day-by-day difference of two series of daily results.
type Paired struct {
	Name  string
	N     int // days with both results
	Total float64
	Mean  float64
	SD    float64
	T     float64 // t statistic of the mean difference against zero
	Wins  int     // days the difference was positive
}

// CalibrationSummary is a decider's calibration over all days.
type CalibrationSummary struct {
	Decider string
	Samples int
	Brier   float64
	Bins    []replay.Bin
}

// CallsSummary scores a decider's BUY and SELL answers at one horizon.
type CallsSummary struct {
	Decider     string
	Label       string // "+5m", "window end" or "day close"
	Scored      int
	Right       int
	HitRate     float64
	MeanMovePct float64
}

// SignalStat relates one signal to the window return of the stocks it was
// read for.
type SignalStat struct {
	Signal     string
	Population string // "all studied stocks" or "picks"
	N          int    // stock-days with the signal
	Pearson    float64
	Spearman   float64
	HasCorr    bool
	// MeanDailyIC is the mean over days of the day's rank correlation, ICDays
	// the days that counted and ICT its t statistic.
	MeanDailyIC float64
	ICDays      int
	ICT         float64
	// ICCI is the 95% interval of the mean daily IC, resampling days.
	ICCI Interval
	// Above, Below and None are the mean return and count of the stocks whose
	// signal was above zero, below zero, and zero or not read.
	Above, Below, None Bucket
}

// Bucket is a group of stock-days and their mean window return.
type Bucket struct {
	N             int
	MeanReturnPct float64
}

// Summarize aggregates day results, which must be in date order.
func Summarize(from, to string, days []DayResult) Summary {
	s := Summary{From: from, To: to, Days: days}
	var ok []DayResult
	for _, d := range days {
		if d.Status == StatusOK {
			ok = append(ok, d)
		} else {
			s.Skipped = append(s.Skipped, d)
		}
	}
	s.OKDays = len(ok)

	picks := make([]ListResult, len(ok))
	for i, d := range ok {
		picks[i] = d.Picks
		s.Usage.Add(d.Jev)
		s.ScreenInputTokens += d.ScreenInputTokens
		s.ScreenOutputTokens += d.ScreenOutputTokens
		for _, n := range d.Notes {
			if strings.HasPrefix(n, "Jev failed on ") || strings.HasPrefix(n, "Jev could not score") {
				s.NewsGaps = append(s.NewsGaps, d.Date+": "+n)
			}
		}
	}
	s.Picks = summarizeList(picks)
	var randoms []ListResult
	for _, d := range ok {
		if d.Random != nil {
			randoms = append(randoms, *d.Random)
		}
	}
	s.Random = summarizeList(randoms)
	s.Deciders = summarizeDeciders(picks)
	s.RandomDeciders = summarizeDeciders(randoms)
	s.Paired = pairs(ok, s.Deciders)
	s.Calibration, s.Calls = calibration(picks)
	s.Signals = signalStats(ok)
	s.Neutral = summarizeNeutral(ok)
	return s
}

func summarizeList(lists []ListResult) ListSummary {
	var s ListSummary
	sum := 0.0
	for _, l := range lists {
		if l.BuyHold.NamesWithData == 0 {
			continue
		}
		s.Days++
		s.Net += l.BuyHold.Net
		s.Gross += l.BuyHold.Gross
		s.Costs += l.BuyHold.Costs
		if l.BuyHold.Net > 0 {
			s.PositiveDays++
		}
		s.Names += l.BuyHold.NamesWithData
		s.NamesUp += l.BuyHold.NamesUp
		sum += l.BuyHold.MeanReturnPct * float64(l.BuyHold.NamesWithData)
	}
	if s.Names > 0 {
		s.UpShare = float64(s.NamesUp) / float64(s.Names)
		s.MeanReturnPct = sum / float64(s.Names)
	}
	return s
}

func summarizeDeciders(lists []ListResult) []DeciderSummary {
	var out []DeciderSummary
	idx := map[string]int{}
	for _, l := range lists {
		for _, r := range l.Runs {
			i, ok := idx[r.Decider]
			if !ok {
				i = len(out)
				idx[r.Decider] = i
				out = append(out, DeciderSummary{Decider: r.Decider})
			}
			d := &out[i]
			d.Days++
			d.NetPnL += r.NetPnL
			d.GrossPnL += r.GrossPnL
			d.Costs += r.Costs
			if r.NetPnL > 0 {
				d.PositiveDays++
			}
			d.Buys += r.Buys
			d.Sells += r.Sells
			d.Holds += r.Holds
			d.HoldSubstitutions += r.HoldSubstitutions
			d.Trades += r.Trades
			d.Wins += r.Wins
			d.WorstDrawdown = math.Max(d.WorstDrawdown, r.MaxDrawdown)
			d.Turnover += r.Turnover
		}
	}
	for i := range out {
		if out[i].Days > 0 {
			out[i].MeanPerDay = out[i].NetPnL / float64(out[i].Days)
		}
		if out[i].Trades > 0 {
			out[i].HitRate = float64(out[i].Wins) / float64(out[i].Trades)
		}
	}
	return out
}

// series is a daily result by date.
type series map[string]float64

func pairedOf(name string, a, b series) (Paired, bool) {
	p := Paired{Name: name}
	var diffs []float64
	for day, x := range a {
		y, ok := b[day]
		if !ok {
			continue
		}
		diffs = append(diffs, x-y)
	}
	if len(diffs) == 0 {
		return p, false
	}
	p.N = len(diffs)
	for _, d := range diffs {
		p.Total += d
		if d > 0 {
			p.Wins++
		}
	}
	p.Mean, p.SD, p.T = mean(diffs), stdev(diffs), tStat(diffs)
	return p, true
}

func pairs(days []DayResult, deciders []DeciderSummary) []Paired {
	picksBH, randomBH := series{}, series{}
	picksRun := map[string]series{}
	randomRun := map[string]series{}
	for _, d := range days {
		if d.Picks.BuyHold.NamesWithData > 0 {
			picksBH[d.Date] = d.Picks.BuyHold.Net
		}
		if d.Random != nil && d.Random.BuyHold.NamesWithData > 0 {
			randomBH[d.Date] = d.Random.BuyHold.Net
		}
		for _, r := range d.Picks.Runs {
			if picksRun[r.Decider] == nil {
				picksRun[r.Decider] = series{}
			}
			picksRun[r.Decider][d.Date] = r.NetPnL
		}
		if d.Random != nil {
			for _, r := range d.Random.Runs {
				if randomRun[r.Decider] == nil {
					randomRun[r.Decider] = series{}
				}
				randomRun[r.Decider][d.Date] = r.NetPnL
			}
		}
	}
	var out []Paired
	add := func(name string, a, b series) {
		if p, ok := pairedOf(name, a, b); ok {
			out = append(out, p)
		}
	}
	add("picks buy-and-hold − random buy-and-hold", picksBH, randomBH)
	for _, d := range deciders {
		if d.Decider == "hold" {
			continue
		}
		add(d.Decider+" on picks − picks buy-and-hold", picksRun[d.Decider], picksBH)
		if d.Decider != "momentum" && picksRun["momentum"] != nil {
			add(d.Decider+" on picks − momentum on picks", picksRun[d.Decider], picksRun["momentum"])
		}
	}
	for d, r := range randomRun {
		if d != "hold" && picksRun[d] != nil {
			add(d+" on picks − "+d+" on random", picksRun[d], r)
		}
	}
	return out
}

func calibration(lists []ListResult) ([]CalibrationSummary, []CallsSummary) {
	type acc struct{ n, pred, real float64 }
	var order []string
	cal := map[string]*CalibrationSummary{}
	merged := map[string]map[int]*acc{}
	type callKey struct{ decider, label string }
	var callOrder []callKey
	calls := map[callKey]*CallsSummary{}
	moves := map[callKey]float64{}
	for _, l := range lists {
		for _, r := range l.Runs {
			c, ok := cal[r.Decider]
			if !ok {
				c = &CalibrationSummary{Decider: r.Decider}
				cal[r.Decider] = c
				merged[r.Decider] = map[int]*acc{}
				order = append(order, r.Decider)
			}
			c.Samples += r.Calibration.Samples
			c.Brier += r.Calibration.Brier * float64(r.Calibration.Samples)
			for _, b := range r.Calibration.Bins {
				i := binIndex(b)
				a := merged[r.Decider][i]
				if a == nil {
					a = &acc{}
					merged[r.Decider][i] = a
				}
				a.n += float64(b.N)
				a.pred += b.Predicted * float64(b.N)
				a.real += b.Realized * float64(b.N)
			}
			for _, s := range r.Calls {
				k := callKey{r.Decider, s.Label}
				cs, ok := calls[k]
				if !ok {
					cs = &CallsSummary{Decider: r.Decider, Label: s.Label}
					calls[k] = cs
					callOrder = append(callOrder, k)
				}
				cs.Scored += s.Scored
				cs.Right += s.Right
				moves[k] += s.MeanMovePct * float64(s.Scored)
			}
		}
	}
	var outCal []CalibrationSummary
	for _, d := range order {
		c := cal[d]
		if c.Samples > 0 {
			c.Brier /= float64(c.Samples)
		}
		idx := make([]int, 0, len(merged[d]))
		for i := range merged[d] {
			idx = append(idx, i)
		}
		slices.Sort(idx)
		for _, i := range idx {
			a := merged[d][i]
			lo := float64(i) / 10
			c.Bins = append(c.Bins, replay.Bin{
				Lo: lo, Hi: lo + 0.1, N: int(a.n), Predicted: a.pred / a.n, Realized: a.real / a.n,
			})
		}
		outCal = append(outCal, *c)
	}
	var outCalls []CallsSummary
	for _, k := range callOrder {
		c := calls[k]
		if c.Scored > 0 {
			c.HitRate = float64(c.Right) / float64(c.Scored)
			c.MeanMovePct = moves[k] / float64(c.Scored)
		}
		outCalls = append(outCalls, *c)
	}
	return outCal, outCalls
}

// binIndex is the decile a calibration bin is in, by its lower edge.
func binIndex(b replay.Bin) int { return int(math.Round(b.Lo * 10)) }

func signalStats(days []DayResult) []SignalStat {
	var out []SignalStat
	for _, population := range []string{"all studied stocks", "picks"} {
		for _, name := range signalNames {
			out = append(out, signalStat(days, name, population))
		}
	}
	return out
}

func signalValue(s StockDay, name string) (float64, bool) {
	if name == "score" {
		return s.Score, true
	}
	v, ok := s.Signals[name]
	return v, ok
}

func signalStat(days []DayResult, name, population string) SignalStat {
	st := SignalStat{Signal: name, Population: population}
	var xs, ys []float64
	var above, below, none []float64
	var ics []float64
	for _, d := range days {
		var dx, dy []float64
		for _, s := range dayStocks(d) {
			if population == "picks" && !s.Picked {
				continue
			}
			v, ok := signalValue(s, name)
			switch {
			case !ok || v == 0:
				none = append(none, s.ReturnPct)
			case v > 0:
				above = append(above, s.ReturnPct)
			default:
				below = append(below, s.ReturnPct)
			}
			if !ok {
				continue
			}
			xs, ys = append(xs, v), append(ys, s.ReturnPct)
			dx, dy = append(dx, v), append(dy, s.ReturnPct)
		}
		if len(dx) >= minDailyNames {
			if rho, ok := spearman(dx, dy); ok {
				ics = append(ics, rho)
			}
		}
	}
	st.N = len(xs)
	st.Pearson, st.HasCorr = pearson(xs, ys)
	st.Spearman, _ = spearman(xs, ys)
	st.MeanDailyIC, st.ICDays, st.ICT, st.ICCI = mean(ics), len(ics), tStat(ics), meanCI(ics)
	st.Above = Bucket{len(above), mean(above)}
	st.Below = Bucket{len(below), mean(below)}
	st.None = Bucket{len(none), mean(none)}
	return st
}
