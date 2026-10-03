package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// DefaultHorizon is how many decision bars ahead a directional call is scored.
const DefaultHorizon = 5

// OptimisticCaveat is printed with every report. Jev's training cutoff is
// unknown, so a backtest over a period it may have seen proves nothing about
// an edge (reports/0002-architecture.md, Risks).
const OptimisticCaveat = "Backtests over any period before the model's release are optimistic upper bounds, " +
	"not evidence of an edge: the model may recognise the episode. Forward paper trading is the deciding evidence."

// Report compares runs over the same data.
type Report struct {
	Label   string       `json:"label"`   // what data the runs replayed
	Horizon int          `json:"horizon"` // bars ahead a directional call is scored
	Runs    []RunMetrics `json:"runs"`
	Notes   []string     `json:"notes,omitempty"`
}

// RunMetrics are one run's results, computed from its decision log.
type RunMetrics struct {
	RunID   string `json:"run_id"`
	Decider string `json:"decider"`

	Cycles            int `json:"cycles"`             // decision cycles
	Buys              int `json:"buys"`               // BUY answers
	Sells             int `json:"sells"`              // SELL answers
	Holds             int `json:"holds"`              // HOLD answers
	HoldSubstitutions int `json:"hold_substitutions"` // HOLDs that replaced a decider failure
	RiskExits         int `json:"risk_exits"`         // risk-driven exit orders
	Orders            int `json:"orders"`             // orders the gate approved and placed
	Fills             int `json:"fills"`

	NetPnL      float64 `json:"net_pnl"`      // after costs, open positions marked
	GrossPnL    float64 `json:"gross_pnl"`    // NetPnL plus costs
	Costs       float64 `json:"costs"`        // brokerage, taxes and fees
	Trades      int     `json:"trades"`       // closed round trips
	Wins        int     `json:"wins"`         // round trips with a positive PnL after costs
	HitRate     float64 `json:"hit_rate"`     // Wins / Trades; 0 when there are no trades
	MaxDrawdown float64 `json:"max_drawdown"` // largest fall of the equity curve from a peak, as a positive number
	Turnover    float64 `json:"turnover"`     // traded notional: quantity times price over all fills

	Calibration Calibration `json:"calibration"`
}

// Calibration compares the probability a decider gave its BUY or SELL with how
// often the price then moved that way over Horizon bars.
type Calibration struct {
	Samples int     `json:"samples"`
	Brier   float64 `json:"brier"` // mean squared gap between probability and outcome; 0 when no samples
	Bins    []Bin   `json:"bins,omitempty"`
}

// Bin is one probability bucket [Lo, Hi).
type Bin struct {
	Lo        float64 `json:"lo"`
	Hi        float64 `json:"hi"`
	N         int     `json:"n"`
	Predicted float64 `json:"predicted"` // mean probability
	Realized  float64 `json:"realized"`  // fraction that moved the predicted way
}

const calibrationBins = 10

// RunRef names a run to evaluate.
type RunRef struct{ ID, Decider string }

// Evaluate computes the report for runs from the decision log. bars are the
// decision-interval bars of the replayed data, used to score calls; interval
// is their width.
func Evaluate(ctx context.Context, log *decisionlog.Log, runs []RunRef, bars []domain.Bar, interval time.Duration, horizon int) (*Report, error) {
	if horizon <= 0 {
		horizon = DefaultHorizon
	}
	fwd := newForward(bars)
	rep := &Report{Horizon: horizon}
	for _, ref := range runs {
		m, err := evaluateRun(ctx, log, ref, fwd, interval, horizon)
		if err != nil {
			return nil, err
		}
		rep.Runs = append(rep.Runs, m)
	}
	return rep, nil
}

func evaluateRun(ctx context.Context, log *decisionlog.Log, ref RunRef, fwd forward, interval time.Duration, horizon int) (RunMetrics, error) {
	m := RunMetrics{RunID: ref.ID, Decider: ref.Decider}
	cycles, err := log.Cycles(ctx, ref.ID)
	if err != nil {
		return m, err
	}
	events, err := log.OrderEvents(ctx, ref.ID)
	if err != nil {
		return m, err
	}
	equity, err := log.Equity(ctx, ref.ID)
	if err != nil {
		return m, err
	}

	var samples []sample
	for _, c := range cycles {
		m.Cycles++
		if c.Intent != nil {
			m.Orders++
			if c.Source == decisionlog.SourceRisk {
				m.RiskExits++
			}
		}
		d := c.Decision
		if d == nil {
			continue
		}
		switch d.Action {
		case domain.ActionBuy:
			m.Buys++
		case domain.ActionSell:
			m.Sells++
		default:
			m.Holds++
		}
		if d.HoldReason != "" {
			m.HoldSubstitutions++
		}
		if d.Action == domain.ActionBuy || d.Action == domain.ActionSell {
			if s, ok := fwd.score(c, interval, horizon); ok {
				samples = append(samples, s)
			}
		}
	}
	m.Calibration = calibrate(samples)

	var fills []domain.Fill
	for _, e := range events {
		if e.Fill != nil {
			fills = append(fills, *e.Fill)
			m.Fills++
			m.Costs += e.Fill.Charges
			m.Turnover += float64(e.Fill.Quantity) * e.Fill.Price
		}
	}
	trades := roundTrips(fills)
	m.Trades = len(trades)
	for _, pnl := range trades {
		if pnl > 0 {
			m.Wins++
		}
	}
	if m.Trades > 0 {
		m.HitRate = float64(m.Wins) / float64(m.Trades)
	}

	curve := make([]float64, 0, len(equity))
	for _, p := range equity {
		curve = append(curve, p.PnL)
	}
	if n := len(curve); n > 0 {
		m.NetPnL = curve[n-1]
	}
	m.GrossPnL = m.NetPnL + m.Costs
	m.MaxDrawdown = maxDrawdown(curve)
	return m, nil
}

// roundTrips returns the PnL after costs of each position that went from flat
// back to flat, in order. A position still open at the end is not a trade.
// Fills must be in execution order.
func roundTrips(fills []domain.Fill) []float64 {
	type book struct {
		qty int64
		avg float64
		pnl float64
	}
	open := map[uint32]*book{}
	var out []float64
	for _, f := range fills {
		b := open[f.Instrument.Token]
		if b == nil {
			b = &book{}
			open[f.Instrument.Token] = b
		}
		signed := f.Quantity
		if f.Side == domain.ActionSell {
			signed = -signed
		}
		next := b.qty + signed
		switch {
		case b.qty == 0 || (b.qty > 0) == (signed > 0):
			b.avg = (math.Abs(float64(b.qty))*b.avg + math.Abs(float64(signed))*f.Price) / math.Abs(float64(next))
		default:
			closed := min(abs(signed), abs(b.qty))
			dir := 1.0
			if b.qty < 0 {
				dir = -1
			}
			b.pnl += float64(closed) * (f.Price - b.avg) * dir
			if next != 0 && (next > 0) != (b.qty > 0) {
				b.avg = f.Price
			}
		}
		b.pnl -= f.Charges
		b.qty = next
		if b.qty == 0 {
			out = append(out, b.pnl)
			delete(open, f.Instrument.Token)
		}
	}
	return out
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// maxDrawdown is the largest fall from a peak of the curve, which starts at
// zero, as a positive number.
func maxDrawdown(curve []float64) float64 {
	peak, dd := 0.0, 0.0
	for _, v := range curve {
		peak = max(peak, v)
		dd = max(dd, peak-v)
	}
	return dd
}

// forward looks up the price some bars after a decision.
type forward map[uint32][]domain.Bar // by token, ordered by end

func newForward(bars []domain.Bar) forward {
	f := forward{}
	for _, b := range bars {
		f[b.Instrument.Token] = append(f[b.Instrument.Token], b)
	}
	for _, bs := range f {
		slices.SortStableFunc(bs, func(a, b domain.Bar) int { return a.End.Compare(b.End) })
	}
	return f
}

type sample struct {
	p       float64 // probability of the called direction
	outcome float64 // 1 if the price moved that way
}

// score judges a BUY or SELL: did the close horizon bars later move the called
// way from the triggering bar's close? A call with no bar that far ahead on
// the same day is not scored.
func (f forward) score(c decisionlog.Cycle, interval time.Duration, horizon int) (sample, bool) {
	if c.Price <= 0 {
		return sample{}, false
	}
	target := c.BarClose.Add(time.Duration(horizon) * interval)
	bs := f[c.Instrument.Token]
	i, _ := slices.BinarySearchFunc(bs, target, func(b domain.Bar, t time.Time) int { return b.End.Compare(t) })
	if i >= len(bs) || !bs[i].End.Equal(target) {
		return sample{}, false
	}
	move := bs[i].Close - c.Price
	if c.Decision.Action == domain.ActionSell {
		move = -move
	}
	s := sample{p: c.Decision.Probability}
	if move > 0 {
		s.outcome = 1
	}
	return s, true
}

func calibrate(samples []sample) Calibration {
	cal := Calibration{Samples: len(samples)}
	if len(samples) == 0 {
		return cal
	}
	type acc struct {
		n          int
		pred, real float64
	}
	var bins [calibrationBins]acc
	var brier float64
	for _, s := range samples {
		brier += (s.p - s.outcome) * (s.p - s.outcome)
		i := min(int(s.p*calibrationBins), calibrationBins-1)
		i = max(i, 0)
		bins[i].n++
		bins[i].pred += s.p
		bins[i].real += s.outcome
	}
	cal.Brier = brier / float64(len(samples))
	for i, b := range bins {
		if b.n == 0 {
			continue
		}
		cal.Bins = append(cal.Bins, Bin{
			Lo: float64(i) / calibrationBins, Hi: float64(i+1) / calibrationBins, N: b.n,
			Predicted: b.pred / float64(b.n), Realized: b.real / float64(b.n),
		})
	}
	return cal
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the report for a terminal.
func (r *Report) WriteText(w io.Writer) error {
	var err error
	printf := func(format string, args ...any) {
		if err == nil {
			_, err = fmt.Fprintf(w, format, args...)
		}
	}
	printf("Evaluation report: %s\n", r.Label)
	printf("PnL is in rupees after brokerage, taxes and fees. Calibration scores BUY/SELL calls %d bars ahead.\n\n", r.Horizon)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	row := func(cells ...string) {
		if err == nil {
			_, err = fmt.Fprintln(tw, strings.Join(cells, "\t")+"\t")
		}
	}
	row("run", "decider", "net PnL", "gross PnL", "costs", "trades", "hit rate", "max DD", "turnover", "orders", "cycles", "B/S/H", "brier")
	for _, m := range r.Runs {
		hit := "n/a"
		if m.Trades > 0 {
			hit = fmt.Sprintf("%.0f%%", m.HitRate*100)
		}
		brier := "n/a"
		if m.Calibration.Samples > 0 {
			brier = fmt.Sprintf("%.3f", m.Calibration.Brier)
		}
		row(m.RunID, m.Decider, money(m.NetPnL), money(m.GrossPnL), money(m.Costs), fmt.Sprint(m.Trades), hit,
			money(m.MaxDrawdown), money(m.Turnover), fmt.Sprint(m.Orders), fmt.Sprint(m.Cycles),
			fmt.Sprintf("%d/%d/%d", m.Buys, m.Sells, m.Holds), brier)
	}
	if err == nil {
		err = tw.Flush()
	}

	for _, m := range r.Runs {
		if m.Calibration.Samples == 0 {
			continue
		}
		printf("\nCalibration, %s (%d scored calls):\n", m.RunID, m.Calibration.Samples)
		printf("  %-11s %6s %10s %9s\n", "probability", "calls", "predicted", "realized")
		for _, b := range m.Calibration.Bins {
			printf("  %.1f - %.1f   %6d %9.1f%% %8.1f%%\n", b.Lo, b.Hi, b.N, b.Predicted*100, b.Realized*100)
		}
	}
	for _, m := range r.Runs {
		if m.HoldSubstitutions > 0 {
			printf("\n%s: %d of %d answers were HOLDs substituted for decider failures.\n", m.RunID, m.HoldSubstitutions, m.Cycles)
		}
	}
	for _, n := range r.Notes {
		printf("\nNote: %s\n", n)
	}
	printf("\n%s\n", OptimisticCaveat)
	return err
}

func money(v float64) string { return fmt.Sprintf("%.2f", v) }
