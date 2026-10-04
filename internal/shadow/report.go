package shadow

import (
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
)

// What a return is netted against.
const (
	// BenchmarkIndex nets the stock's move against the index the run watched
	// (NIFTY 50 by default) over the same interval.
	BenchmarkIndex = "index"
	// BenchmarkUniverse nets it against the mean move of the day's screened
	// universe: every stock the day's signals were about, as far as it was
	// recorded. It is the picker's own cross-section, so over all the picks of
	// a day it is zero by construction; it tells signals apart from each
	// other, not from the picker's list.
	BenchmarkUniverse = "universe"
	// BenchmarkNone leaves the raw move.
	BenchmarkNone = "none"
)

// Benchmarks are the valid values of Options.Benchmark.
var Benchmarks = []string{BenchmarkIndex, BenchmarkUniverse, BenchmarkNone}

// Options configure BuildReport.
type Options struct {
	From, To  string // the IST days, inclusive
	Benchmark string // one of Benchmarks; empty is BenchmarkIndex
	// ByVerdict adds a row group per risk-gate verdict of each decision signal.
	ByVerdict bool
	// Resamples is how many day resamples the confidence interval uses; zero
	// means DefaultResamples. Seed makes them repeatable.
	Resamples int
	Seed      uint64
}

// DefaultResamples is the number of day resamples of the confidence interval.
const DefaultResamples = 2000

// Row is one signal group at one horizon.
type Row struct {
	Group   string `json:"group"`
	Horizon string `json:"horizon"`
	N       int    `json:"n"`    // outcomes
	Days    int    `json:"days"` // distinct IST days among them
	// MeanPct is the mean return in the direction the signal bet on, net of
	// the benchmark, in percent: positive means the signal was right on
	// average. CILow and CIHigh are its 95% interval from resampling whole
	// days, and are absent (HasCI false) with fewer than two days.
	MeanPct float64 `json:"mean_pct"`
	CILow   float64 `json:"ci_low_pct"`
	CIHigh  float64 `json:"ci_high_pct"`
	HasCI   bool    `json:"has_ci"`
	// HitRate is the share of outcomes whose net return in the signal's
	// direction was above zero.
	HitRate float64 `json:"hit_rate"`
}

// Report is the shadow log's summary over a range of days.
type Report struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Benchmark string `json:"benchmark"`
	Signals   int    `json:"signals"`
	// Days are the IST days with a signal.
	Days int `json:"days"`
	// Awaiting counts the outcomes that apply but are not filled yet, and
	// Unbenchmarked those left out because the benchmark was unknown for them.
	Awaiting      int   `json:"awaiting"`
	Unbenchmarked int   `json:"unbenchmarked"`
	Rows          []Row `json:"rows"`
}

// obs is one outcome of one group.
type obs struct {
	day string
	ret float64 // net return in the signal's direction
}

type cell struct{ group, horizon string }

// BuildReport summarises signals and their outcomes per signal group and
// horizon.
func BuildReport(signals []decisionlog.ShadowSignal, outcomes []decisionlog.ShadowOutcome, opt Options) (Report, error) {
	bench := opt.Benchmark
	if bench == "" {
		bench = BenchmarkIndex
	}
	if !slices.Contains(Benchmarks, bench) {
		return Report{}, fmt.Errorf("shadow: benchmark %q is not one of %s", bench, strings.Join(Benchmarks, ", "))
	}
	rep := Report{From: opt.From, To: opt.To, Benchmark: bench, Signals: len(signals)}
	byID := make(map[int64]decisionlog.ShadowSignal, len(signals))
	days := map[string]bool{}
	var order []string // group labels in first-seen order
	seenGroup := map[string]bool{}
	for _, s := range signals {
		byID[s.ID] = s
		days[s.Day] = true
		for _, g := range groups(s, opt.ByVerdict) {
			if !seenGroup[g] {
				seenGroup[g] = true
				order = append(order, g)
			}
		}
	}
	rep.Days = len(days)

	cells := map[cell][]obs{}
	has := map[outcomeKey]bool{}
	for _, o := range outcomes {
		s, ok := byID[o.SignalID]
		if !ok {
			continue
		}
		has[outcomeKey{o.SignalID, o.Horizon}] = true
		net := o.ReturnPct
		switch bench {
		case BenchmarkIndex:
			if o.IndexPct == nil {
				rep.Unbenchmarked++
				continue
			}
			net -= *o.IndexPct
		case BenchmarkUniverse:
			if o.UniversePct == nil {
				rep.Unbenchmarked++
				continue
			}
			net -= *o.UniversePct
		}
		net *= float64(s.Direction)
		for _, g := range groups(s, opt.ByVerdict) {
			c := cell{g, o.Horizon}
			cells[c] = append(cells[c], obs{s.Day, net})
		}
	}
	for _, s := range signals {
		for _, h := range Horizons {
			if _, ok := target(s, h); ok && !has[outcomeKey{s.ID, h}] {
				rep.Awaiting++
			}
		}
	}

	resamples := opt.Resamples
	if resamples <= 0 {
		resamples = DefaultResamples
	}
	for _, g := range order {
		for _, h := range Horizons {
			list := cells[cell{g, h}]
			if len(list) == 0 {
				continue
			}
			rep.Rows = append(rep.Rows, summarize(g, h, list, resamples, opt.Seed))
		}
	}
	return rep, nil
}

// groups are the report groups a signal counts in: its own label; for a
// decision, whether the gate let it through; and with byVerdict, the gate's
// verdict.
func groups(s decisionlog.ShadowSignal, byVerdict bool) []string {
	out := []string{s.Signal}
	if s.Kind != decisionlog.ShadowDecision || s.Action == "HOLD" {
		return out
	}
	if s.Traded {
		out = append(out, s.Signal+" traded")
	} else {
		out = append(out, s.Signal+" not traded")
	}
	if byVerdict && s.Verdict != "" {
		out = append(out, s.Signal+" ["+s.Verdict+"]")
	}
	return out
}

func summarize(group, horizon string, list []obs, resamples int, seed uint64) Row {
	r := Row{Group: group, Horizon: horizon, N: len(list)}
	byDay := map[string][]float64{}
	var sum float64
	hits := 0
	for _, o := range list {
		byDay[o.day] = append(byDay[o.day], o.ret)
		sum += o.ret
		if o.ret > 0 {
			hits++
		}
	}
	r.Days = len(byDay)
	r.MeanPct = sum / float64(len(list))
	r.HitRate = float64(hits) / float64(len(list))
	if r.Days >= 2 {
		r.CILow, r.CIHigh = dayBootstrap(byDay, resamples, seed)
		r.HasCI = true
	}
	return r
}

// dayBootstrap is the 95% percentile interval of the pooled mean when whole
// days are resampled with replacement. Outcomes of one day share the market
// that day and are not independent, so the day, not the outcome, is the unit
// that is resampled.
func dayBootstrap(byDay map[string][]float64, resamples int, seed uint64) (lo, hi float64) {
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	slices.Sort(keys) // map order is random; the resampling must not depend on it
	sums := make([]float64, len(keys))
	for i, k := range keys {
		for _, v := range byDay[k] {
			sums[i] += v
		}
	}
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	means := make([]float64, resamples)
	for b := range means {
		var sum float64
		n := 0
		for range keys {
			i := rng.IntN(len(keys))
			sum += sums[i]
			n += len(byDay[keys[i]])
		}
		means[b] = sum / float64(n)
	}
	slices.Sort(means)
	idx := func(q float64) int { return min(len(means)-1, max(0, int(math.Floor(q*float64(len(means)))))) }
	return means[idx(0.025)], means[idx(0.975)]
}

// Write prints the report as text.
func (r Report) Write(w io.Writer) error {
	fmt.Fprintf(w, "Shadow log %s to %s: %d signals on %d days, returns net of %s\n", r.From, r.To, r.Signals, r.Days, benchmarkText(r.Benchmark))
	fmt.Fprintln(w, "MEAN is the return in the direction the signal bet on (a SELL bets on a fall; a pick, a BUY and a HOLD on a rise), in percent;")
	fmt.Fprintln(w, "CI95 resamples whole days; HIT is the share of outcomes above zero.")
	if r.Awaiting > 0 || r.Unbenchmarked > 0 {
		fmt.Fprintf(w, "Not counted: %d outcomes the recorded bars do not reach yet, %d without a benchmark.\n", r.Awaiting, r.Unbenchmarked)
	}
	fmt.Fprintln(w)
	if len(r.Rows) == 0 {
		_, err := fmt.Fprintln(w, "no outcomes in range")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SIGNAL\tHORIZON\tN\tDAYS\tMEAN %\tCI95 %\tHIT %")
	last := ""
	for _, row := range r.Rows {
		name := row.Group
		if name == last {
			name = ""
		}
		last = row.Group
		ci := "n/a (needs 2 days)"
		if row.HasCI {
			ci = fmt.Sprintf("%+.3f to %+.3f", row.CILow, row.CIHigh)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%+.3f\t%s\t%.1f\n", name, row.Horizon, row.N, row.Days, row.MeanPct, ci, row.HitRate*100)
	}
	return tw.Flush()
}

func benchmarkText(b string) string {
	switch b {
	case BenchmarkIndex:
		return "the index over the same interval"
	case BenchmarkUniverse:
		return "the mean of the day's recorded universe"
	}
	return "nothing (raw moves)"
}
