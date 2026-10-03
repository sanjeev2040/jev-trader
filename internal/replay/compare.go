package replay

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
)

var ist = time.FixedZone("IST", 5*3600+30*60)

// sessionOpenOffset is the NSE session open, as an offset from IST midnight.
const sessionOpenOffset = 9*time.Hour + 15*time.Minute

// Baseline decider names, which the comparison reads off the runs.
const (
	baselineHold     = "hold"
	baselineMomentum = "momentum"
)

// dayWindow is the trading window of each IST day, as offsets from IST
// midnight. A zero End is the end of the data.
type dayWindow struct{ Start, End time.Duration }

func istMidnight(t time.Time) time.Time {
	y, m, d := t.In(ist).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, ist)
}

// startOn is when the window opens on the IST day of t; never before the
// session open, which is where the engine's day starts.
func (w dayWindow) startOn(t time.Time) time.Time {
	return istMidnight(t).Add(max(w.Start, sessionOpenOffset))
}

// endOn is when the window closes on the IST day of t, or the end of that day.
func (w dayWindow) endOn(t time.Time) time.Time {
	if w.End == 0 {
		return istMidnight(t).Add(24 * time.Hour)
	}
	return istMidnight(t).Add(w.End)
}

// String is the window as "09:20-10:30 IST", or "" for the whole day.
func (w dayWindow) String() string {
	if w.Start == 0 && w.End == 0 {
		return ""
	}
	clock := func(d time.Duration) string { return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60) }
	switch {
	case w.End == 0:
		return "from " + clock(w.Start) + " IST"
	case w.Start == 0:
		return "until " + clock(w.End) + " IST"
	}
	return clock(w.Start) + "-" + clock(w.End) + " IST"
}

// closeAt returns the close of the last bar of token that ended at or before t.
func (f forward) closeAt(token uint32, t time.Time) (float64, bool) {
	bs := f[token]
	i, _ := slices.BinarySearchFunc(bs, t, func(b domain.Bar, t time.Time) int { return b.End.Compare(t) })
	// i is the first bar ending at or after t.
	if i < len(bs) && bs[i].End.Equal(t) {
		return bs[i].Close, true
	}
	if i == 0 {
		return 0, false
	}
	return bs[i-1].Close, true
}

// priceAt is the price of token at target, the close of the last bar that
// ended by then, for a call made at from. It is unknown when the data stops
// before target on that day: the market was not observed that far ahead.
func (f forward) priceAt(token uint32, from, target time.Time) (float64, bool) {
	bs := f[token]
	i, _ := slices.BinarySearchFunc(bs, target, func(b domain.Bar, t time.Time) int { return b.End.Compare(t) })
	if i >= len(bs) || istMidnight(bs[i].End) != istMidnight(target) {
		return 0, false
	}
	if bs[i].End.Equal(target) {
		return bs[i].Close, true
	}
	if i == 0 || bs[i-1].End.Before(from) {
		return 0, false
	}
	return bs[i-1].Close, true
}

// Comparison sets the runs against what the market then did.
type Comparison struct {
	// LookaheadMinutes are the horizons each call is checked at.
	LookaheadMinutes []int `json:"lookahead_minutes"`
	// Calls is every BUY and SELL answer, with the real price afterwards.
	Calls []CallOutcome `json:"calls"`
	// Summary scores the calls per run and horizon.
	Summary []CallSummary `json:"summary"`
	BuyHold BuyHold       `json:"buy_hold"`
	// PnL puts each run beside buy-and-hold and the baselines.
	PnL []RunPnL `json:"pnl"`
}

// CallOutcome is one BUY or SELL answer and the price that followed it.
type CallOutcome struct {
	RunID       string    `json:"run_id"`
	Decider     string    `json:"decider"`
	Time        time.Time `json:"time"` // close of the bar the call was made on
	Instrument  string    `json:"instrument"`
	Action      string    `json:"action"`
	Probability float64   `json:"probability"`
	Verdict     string    `json:"verdict"`  // the risk gate's reason code
	Executed    bool      `json:"executed"` // the gate approved an order
	Price       float64   `json:"price"`    // close of that bar
	Outcomes    []Outcome `json:"outcomes"` // one per horizon, then window end and day close
}

// Outcome is the real price at some point after a call. Price and MovePct are
// nil when the data does not reach that point. MovePct is the percentage move
// from the call's price in the direction the call bet on: positive means the
// call was right.
type Outcome struct {
	Label   string   `json:"label"` // "+5m", "window end" or "day close"
	Price   *float64 `json:"price,omitempty"`
	MovePct *float64 `json:"move_pct,omitempty"`
}

// CallSummary scores one run's calls at one horizon.
type CallSummary struct {
	RunID       string  `json:"run_id"`
	Label       string  `json:"label"`
	Calls       int     `json:"calls"`
	Scored      int     `json:"scored"`        // calls with a price at this point
	Right       int     `json:"right"`         // calls whose price then moved the called way
	MeanMovePct float64 `json:"mean_move_pct"` // mean MovePct of the scored calls
}

// BuyHold is the benchmark of buying every instrument at the window's open
// and holding it to the window's end.
type BuyHold struct {
	// Notional is what each instrument is bought for, per day.
	Notional float64      `json:"notional"`
	Rows     []BuyHoldRow `json:"rows"`
	Gross    float64      `json:"gross"`
	Costs    float64      `json:"costs"`
	Net      float64      `json:"net"`
}

// BuyHoldRow is the benchmark for one instrument on one day.
type BuyHoldRow struct {
	Day        string   `json:"day"`
	Instrument string   `json:"instrument"`
	Quantity   int64    `json:"quantity"`
	StartPrice float64  `json:"start_price"` // open of the window's first bar
	EndPrice   float64  `json:"end_price"`   // close of its last bar
	DayClose   *float64 `json:"day_close,omitempty"`
	ReturnPct  float64  `json:"return_pct"`
	Gross      float64  `json:"gross"`
	Costs      float64  `json:"costs"` // buying at the start and selling at the end
	Net        float64  `json:"net"`
}

// RunPnL is a run's net PnL over the window, and how far it is ahead of the
// benchmarks. A Vs field is nil when that benchmark was not part of the replay.
type RunPnL struct {
	RunID      string   `json:"run_id"`
	Decider    string   `json:"decider"`
	Baseline   bool     `json:"baseline"`
	NetPnL     float64  `json:"net_pnl"`
	GrossPnL   float64  `json:"gross_pnl"`
	Costs      float64  `json:"costs"`
	VsBuyHold  float64  `json:"vs_buy_hold"`
	VsHold     *float64 `json:"vs_hold,omitempty"`
	VsMomentum *float64 `json:"vs_momentum,omitempty"`
}

// CompareConfig configures BuildComparison.
type CompareConfig struct {
	Lookahead  []time.Duration
	Window     dayWindow
	IndexToken uint32
	// Notional is what the buy-and-hold benchmark buys per instrument and day.
	Notional float64
	Costs    paper.CostModel
}

// BuildComparison compares runs with the market that followed them. tape is
// the whole replayed data, including what came after the window.
func BuildComparison(ctx context.Context, log *decisionlog.Log, runs []RunRef, metrics []RunMetrics, tape forward, cfg CompareConfig) (*Comparison, error) {
	cmp := &Comparison{}
	for _, d := range cfg.Lookahead {
		cmp.LookaheadMinutes = append(cmp.LookaheadMinutes, int(d/time.Minute))
	}
	for _, ref := range runs {
		cycles, err := log.Cycles(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		calls := callOutcomes(ref, cycles, tape, cfg)
		cmp.Calls = append(cmp.Calls, calls...)
		cmp.Summary = append(cmp.Summary, summarize(ref, calls, cfg)...)
	}
	cmp.BuyHold = buyAndHold(tape, cfg)

	var hold, momentum *float64
	for _, m := range metrics {
		switch m.Decider {
		case baselineHold:
			hold = &m.NetPnL
		case baselineMomentum:
			momentum = &m.NetPnL
		}
	}
	for _, m := range metrics {
		p := RunPnL{
			RunID: m.RunID, Decider: m.Decider, Baseline: m.Decider == baselineHold || m.Decider == baselineMomentum,
			NetPnL: m.NetPnL, GrossPnL: m.GrossPnL, Costs: m.Costs, VsBuyHold: round2(m.NetPnL - cmp.BuyHold.Net),
		}
		if hold != nil {
			v := round2(m.NetPnL - *hold)
			p.VsHold = &v
		}
		if momentum != nil {
			v := round2(m.NetPnL - *momentum)
			p.VsMomentum = &v
		}
		cmp.PnL = append(cmp.PnL, p)
	}
	return cmp, nil
}

func callOutcomes(ref RunRef, cycles []decisionlog.Cycle, tape forward, cfg CompareConfig) []CallOutcome {
	var out []CallOutcome
	for _, c := range cycles {
		d := c.Decision
		if d == nil || (d.Action != domain.ActionBuy && d.Action != domain.ActionSell) {
			continue
		}
		call := CallOutcome{
			RunID: ref.ID, Decider: ref.Decider, Time: c.BarClose, Instrument: label(c.Instrument),
			Action: string(d.Action), Probability: d.Probability,
			Verdict: c.Verdict, Executed: c.Intent != nil, Price: c.Price,
		}
		token := c.Instrument.Token
		outcome := func(lbl string, target time.Time) {
			o := Outcome{Label: lbl}
			if p, ok := tape.priceAt(token, c.BarClose, target); ok && c.Price > 0 {
				move := (p - c.Price) / c.Price * 100
				if d.Action == domain.ActionSell {
					move = -move
				}
				o.Price, o.MovePct = &p, &move
			}
			call.Outcomes = append(call.Outcomes, o)
		}
		for _, la := range cfg.Lookahead {
			outcome(fmt.Sprintf("+%dm", int(la/time.Minute)), c.BarClose.Add(la))
		}
		if cfg.Window.End != 0 {
			outcome("window end", cfg.Window.endOn(c.BarClose))
		}
		outcome("day close", dayClose(tape, token, c.BarClose))
		out = append(out, call)
	}
	return out
}

// dayClose is the end of the last bar token has on the IST day of t.
func dayClose(tape forward, token uint32, t time.Time) time.Time {
	bs := tape[token]
	limit := istMidnight(t).Add(24 * time.Hour)
	i, _ := slices.BinarySearchFunc(bs, limit, func(b domain.Bar, t time.Time) int { return b.End.Compare(t) })
	if i == 0 {
		return t
	}
	return bs[i-1].End
}

func summarize(ref RunRef, calls []CallOutcome, cfg CompareConfig) []CallSummary {
	if len(calls) == 0 {
		return nil
	}
	var out []CallSummary
	for i := range calls[0].Outcomes {
		s := CallSummary{RunID: ref.ID, Label: calls[0].Outcomes[i].Label, Calls: len(calls)}
		var sum float64
		for _, c := range calls {
			if m := c.Outcomes[i].MovePct; m != nil {
				s.Scored++
				sum += *m
				if *m > 0 {
					s.Right++
				}
			}
		}
		if s.Scored > 0 {
			s.MeanMovePct = sum / float64(s.Scored)
		}
		out = append(out, s)
	}
	return out
}

func buyAndHold(tape forward, cfg CompareConfig) BuyHold {
	bh := BuyHold{Notional: cfg.Notional}
	tokens := make([]uint32, 0, len(tape))
	for token := range tape {
		if token != cfg.IndexToken {
			tokens = append(tokens, token)
		}
	}
	slices.Sort(tokens)
	for _, token := range tokens {
		var first, last *domain.Bar
		flush := func() {
			if first == nil {
				return
			}
			start, end := first.Open, last.Close
			qty := int64(0)
			if start > 0 {
				qty = int64(cfg.Notional / start)
			}
			if qty > 0 {
				row := BuyHoldRow{
					Day: first.Start.In(ist).Format(time.DateOnly), Instrument: label(first.Instrument),
					Quantity: qty, StartPrice: start, EndPrice: end, ReturnPct: (end - start) / start * 100,
					Gross: float64(qty) * (end - start),
					Costs: cfg.Costs.Charges(domain.ActionBuy, qty, start) + cfg.Costs.Charges(domain.ActionSell, qty, end),
				}
				row.Net = row.Gross - row.Costs
				if p, ok := tape.closeAt(token, dayClose(tape, token, first.Start)); ok {
					row.DayClose = &p
				}
				row.Gross, row.Costs, row.Net = round2(row.Gross), round2(row.Costs), round2(row.Net)
				bh.Rows = append(bh.Rows, row)
				bh.Gross += row.Gross
				bh.Costs += row.Costs
				bh.Net += row.Net
			}
			first, last = nil, nil
		}
		for i := range tape[token] {
			b := &tape[token][i]
			inWindow := !b.Start.Before(cfg.Window.startOn(b.Start)) && !b.End.After(cfg.Window.endOn(b.Start))
			if first != nil && istMidnight(b.Start) != istMidnight(first.Start) {
				flush()
			}
			if !inWindow {
				continue
			}
			if first == nil {
				first = b
			}
			last = b
		}
		flush()
	}
	bh.Gross, bh.Costs, bh.Net = round2(bh.Gross), round2(bh.Costs), round2(bh.Net)
	return bh
}

func (c *Comparison) writeText(w io.Writer) error {
	var err error
	printf := func(format string, args ...any) {
		if err == nil {
			_, err = fmt.Fprintf(w, format, args...)
		}
	}
	printf("\nComparison with the real market\n")
	printf("Prices are the replayed data's bar closes. A move is the percentage change from the call's price in the\n")
	printf("direction the call bet on, so positive means the call was right.\n")

	byRun := map[string][]CallOutcome{}
	var runOrder []string
	for _, call := range c.Calls {
		if _, ok := byRun[call.RunID]; !ok {
			runOrder = append(runOrder, call.RunID)
		}
		byRun[call.RunID] = append(byRun[call.RunID], call)
	}
	for _, id := range runOrder {
		calls := byRun[id]
		printf("\nBUY/SELL calls, %s (%s):\n", id, calls[0].Decider)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		header := []string{"time", "instrument", "call", "p", "gate", "price"}
		for _, o := range calls[0].Outcomes {
			header = append(header, o.Label)
		}
		fmt.Fprintln(tw, "  "+strings.Join(header, "\t"))
		for _, call := range calls {
			cells := []string{
				call.Time.In(ist).Format("01-02 15:04:05"), call.Instrument, call.Action,
				fmt.Sprintf("%.2f", call.Probability), call.Verdict, fmt.Sprintf("%.2f", call.Price),
			}
			for _, o := range call.Outcomes {
				if o.Price == nil {
					cells = append(cells, "n/a")
					continue
				}
				cells = append(cells, fmt.Sprintf("%.2f (%+.2f%%)", *o.Price, zeroSmall(*o.MovePct)))
			}
			fmt.Fprintln(tw, "  "+strings.Join(cells, "\t"))
		}
		if err == nil {
			err = tw.Flush()
		}
		for _, s := range c.Summary {
			if s.RunID == id && s.Scored > 0 {
				printf("  %-10s %d of %d scored calls moved the called way, mean move %+.2f%%\n", s.Label+":", s.Right, s.Scored, zeroSmall(s.MeanMovePct))
			}
		}
	}
	if len(runOrder) == 0 {
		printf("\nNo run made a BUY or SELL call.\n")
	}

	printf("\nBuy-and-hold benchmark: each instrument bought at the window's first open for about %.0f and held to its last close.\n", c.BuyHold.Notional)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "  day\tinstrument\tqty\tstart\tend\treturn\tgross\tcosts\tnet\tday close\t")
	for _, r := range c.BuyHold.Rows {
		dc := "n/a"
		if r.DayClose != nil {
			dc = fmt.Sprintf("%.2f", *r.DayClose)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%.2f\t%.2f\t%+.2f%%\t%.2f\t%.2f\t%.2f\t%s\t\n", r.Day, r.Instrument, r.Quantity, r.StartPrice, r.EndPrice, zeroSmall(r.ReturnPct), r.Gross, r.Costs, r.Net, dc)
	}
	fmt.Fprintf(tw, "  \ttotal\t\t\t\t\t%.2f\t%.2f\t%.2f\t\t\n", c.BuyHold.Gross, c.BuyHold.Costs, c.BuyHold.Net)
	if err == nil {
		err = tw.Flush()
	}

	printf("\nNet PnL over the window, after costs, against the benchmarks (a positive difference is ahead):\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "  run\tdecider\tnet PnL\tvs buy-and-hold\tvs always-HOLD\tvs momentum\t")
	fmt.Fprintf(tw, "  buy-and-hold\t\t%.2f\t\t\t\t\n", c.BuyHold.Net)
	diff := func(v *float64) string {
		if v == nil {
			return "n/a"
		}
		return signed(*v)
	}
	for _, p := range c.PnL {
		fmt.Fprintf(tw, "  %s\t%s\t%.2f\t%+.2f\t%s\t%s\t\n", p.RunID, p.Decider, p.NetPnL, zeroSmall(p.VsBuyHold), diff(p.VsHold), diff(p.VsMomentum))
	}
	if err == nil {
		err = tw.Flush()
	}
	return err
}
