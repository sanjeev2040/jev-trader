package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

// WalkMode says how much of a run's walkthrough to keep.
type WalkMode string

// Walkthrough modes. Off is the zero value.
const (
	WalkOff     WalkMode = ""
	WalkActions WalkMode = "actions" // BUY and SELL answers, risk exits, fills, and failed decisions
	WalkAll     WalkMode = "all"     // every decision, HOLDs included
)

// Row kinds.
const (
	RowDecision = "decision"
	RowRiskExit = "risk_exit"
	RowFill     = "fill"
)

// RunWalk is one run's session, instrument by instrument.
type RunWalk struct {
	RunID       string           `json:"run_id"`
	Decider     string           `json:"decider"`
	Instruments []InstrumentWalk `json:"instruments"`
}

// InstrumentWalk is what one instrument went through in a run, in time order.
type InstrumentWalk struct {
	Instrument string `json:"instrument"` // EXCHANGE:SYMBOL, or "token N" when the stored data has no symbol
	Token      uint32 `json:"token"`

	Decisions int `json:"decisions"`
	Buys      int `json:"buys"`
	Sells     int `json:"sells"`
	Holds     int `json:"holds"`
	Fills     int `json:"fills"`

	// Position and AvgPrice are what the run held at the end; NetPnL is its
	// profit after costs with that position marked at the window's last price.
	Position int64   `json:"position"`
	AvgPrice float64 `json:"avg_price,omitempty"`
	NetPnL   float64 `json:"net_pnl"`

	Rows []WalkRow `json:"rows"`
}

// WalkRow is one step: a decision, a risk-driven exit or a fill. Position,
// AvgPrice and PnL are the instrument's running state right after the step,
// the PnL after costs with the position marked at Price.
type WalkRow struct {
	Time  time.Time `json:"time"` // bar close for a decision, execution time for a fill
	Kind  string    `json:"kind"`
	Price float64   `json:"price"` // bar close, or the fill price

	// Decision.
	Action        string        `json:"action,omitempty"`
	Probability   float64       `json:"probability,omitempty"`
	Size          string        `json:"size,omitempty"`
	Confidence    float64       `json:"confidence,omitempty"`
	HoldReason    string        `json:"hold_reason,omitempty"` // set when a HOLD replaced a decider failure
	Features      *WalkFeatures `json:"features,omitempty"`
	Verdict       string        `json:"verdict,omitempty"` // the risk gate's reason code
	VerdictDetail string        `json:"verdict_detail,omitempty"`
	Order         *WalkOrder    `json:"order,omitempty"` // the order the gate approved
	ExitReason    string        `json:"exit_reason,omitempty"`

	// Fill.
	Side     string  `json:"side,omitempty"`
	Quantity int64   `json:"quantity,omitempty"`
	Charges  float64 `json:"charges,omitempty"`

	Position int64   `json:"position"`
	AvgPrice float64 `json:"avg_price,omitempty"`
	PnL      float64 `json:"pnl"`
}

// WalkOrder is an approved order.
type WalkOrder struct {
	Side     string  `json:"side"`
	Quantity int64   `json:"quantity"`
	RefPrice float64 `json:"ref_price,omitempty"`
}

// WalkFeatures are the snapshot values that mattered most to a decision; the
// pointers are nil when the snapshot had too little history to compute them.
type WalkFeatures struct {
	MinutesSinceOpen int      `json:"minutes_since_open"`
	Return1Pct       *float64 `json:"return_1_pct"`
	Return5Pct       *float64 `json:"return_5_pct"`
	Return15Pct      *float64 `json:"return_15_pct"`
	SessionReturnPct float64  `json:"session_return_pct"`
	VWAPDistPct      *float64 `json:"vwap_dist_pct"`
	VolumeRel        *float64 `json:"volume_rel"`
	SessionRangePos  *float64 `json:"session_range_pos"`
	IndexSessionPct  *float64 `json:"index_session_return_pct"` // nil when the run had no index context
	PositionSide     string   `json:"position_side"`
}

func featuresOf(raw json.RawMessage) *WalkFeatures {
	var s snapshot.Snapshot
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return nil
	}
	f := &WalkFeatures{
		MinutesSinceOpen: s.Session.MinutesSinceOpen,
		Return1Pct:       s.Features.ReturnsPct.Bars1,
		Return5Pct:       s.Features.ReturnsPct.Bars5,
		Return15Pct:      s.Features.ReturnsPct.Bars15,
		SessionReturnPct: s.Features.SessionReturnPct,
		VWAPDistPct:      s.Features.VWAPDistPct,
		VolumeRel:        s.Features.VolumeRel,
		SessionRangePos:  s.Features.SessionRangePos,
		PositionSide:     s.Position.Side,
	}
	if s.Index.Available {
		v := s.Index.SessionReturnPct
		f.IndexSessionPct = &v
	}
	return f
}

// label names an instrument for people.
func label(in domain.Instrument) string {
	if in.Symbol == "" {
		return fmt.Sprintf("token %d", in.Token)
	}
	if in.Exchange == "" {
		return in.Symbol
	}
	return in.Exchange + ":" + in.Symbol
}

// posBook tracks one instrument's position and PnL from its fills.
type posBook struct {
	qty      int64
	avg      float64
	realized float64 // after costs
}

func (b *posBook) apply(f domain.Fill) {
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
		b.realized += float64(closed) * (f.Price - b.avg) * dir
		if next != 0 && (next > 0) != (b.qty > 0) {
			b.avg = f.Price
		}
	}
	b.realized -= f.Charges
	b.qty = next
	if b.qty == 0 {
		b.avg = 0
	}
}

func (b *posBook) pnl(mark float64) float64 {
	return b.realized + float64(b.qty)*(mark-b.avg)
}

// walkEvent is a cycle or a fill awaiting its place in time.
type walkEvent struct {
	at    time.Time
	order int // fills sort before the decision of the same instant, as the engine applies them
	seq   int
	cycle *decisionlog.Cycle
	fill  *domain.Fill
}

// BuildWalkthrough turns a run's decision log into a chronological walk,
// instrument by instrument. tape supplies the prices open positions are
// marked at when the walk ends; it may be nil.
func BuildWalkthrough(ctx context.Context, log *decisionlog.Log, ref RunRef, mode WalkMode, tape forward, window dayWindow) (RunWalk, error) {
	walk := RunWalk{RunID: ref.ID, Decider: ref.Decider}
	cycles, err := log.Cycles(ctx, ref.ID)
	if err != nil {
		return walk, err
	}
	events, err := log.OrderEvents(ctx, ref.ID)
	if err != nil {
		return walk, err
	}

	byToken := map[uint32][]walkEvent{}
	insts := map[uint32]domain.Instrument{}
	var order []uint32
	add := func(in domain.Instrument, ev walkEvent) {
		if _, ok := insts[in.Token]; !ok {
			order = append(order, in.Token)
		}
		if cur := insts[in.Token]; cur.Symbol == "" {
			insts[in.Token] = in
		}
		byToken[in.Token] = append(byToken[in.Token], ev)
	}
	for i := range cycles {
		c := &cycles[i]
		add(c.Instrument, walkEvent{at: c.BarClose, order: 1, seq: i, cycle: c})
	}
	for i, e := range events {
		if e.Fill != nil {
			add(e.Instrument, walkEvent{at: e.Time, order: 0, seq: i, fill: e.Fill})
		}
	}

	for _, token := range order {
		evs := byToken[token]
		slices.SortStableFunc(evs, func(a, b walkEvent) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			if a.order != b.order {
				return a.order - b.order
			}
			return a.seq - b.seq
		})
		iw := InstrumentWalk{Instrument: label(insts[token]), Token: token}
		var book posBook
		var mark float64
		for _, ev := range evs {
			row := WalkRow{Time: ev.at}
			switch {
			case ev.fill != nil:
				f := ev.fill
				book.apply(*f)
				mark = f.Price
				row.Kind, row.Price, row.Side, row.Quantity, row.Charges = RowFill, f.Price, string(f.Side), f.Quantity, f.Charges
				iw.Fills++
			case ev.cycle.Decision == nil:
				c := ev.cycle
				if c.Price > 0 {
					mark = c.Price
				}
				row.Kind, row.Price, row.ExitReason = RowRiskExit, c.Price, c.ExitReason
				row.Verdict, row.VerdictDetail = c.Verdict, c.VerdictDetail
				row.Order = walkOrder(c.Intent)
			default:
				c := ev.cycle
				d := c.Decision
				mark = c.Price
				row.Kind, row.Price = RowDecision, c.Price
				row.Action, row.Probability, row.Size, row.Confidence, row.HoldReason = string(d.Action), d.Probability, d.Size, d.Confidence, d.HoldReason
				row.Features = featuresOf(c.Snapshot)
				row.Verdict, row.VerdictDetail = c.Verdict, c.VerdictDetail
				row.Order = walkOrder(c.Intent)
				iw.Decisions++
				switch d.Action {
				case domain.ActionBuy:
					iw.Buys++
				case domain.ActionSell:
					iw.Sells++
				default:
					iw.Holds++
				}
				if mode != WalkAll && d.Action == domain.ActionHold && d.HoldReason == "" {
					// Keep the running state moving, but not the row.
					continue
				}
			}
			row.Position, row.AvgPrice, row.PnL = book.qty, book.avg, round2(book.pnl(mark))
			iw.Rows = append(iw.Rows, row)
		}
		// Mark what is still open at the window's last price.
		if last := evs[len(evs)-1].at; book.qty != 0 {
			if p, ok := tape.closeAt(token, window.endOn(last)); ok {
				mark = p
			}
		}
		iw.Position, iw.AvgPrice, iw.NetPnL = book.qty, book.avg, round2(book.pnl(mark))
		walk.Instruments = append(walk.Instruments, iw)
	}
	return walk, nil
}

func walkOrder(in *domain.OrderIntent) *WalkOrder {
	if in == nil {
		return nil
	}
	return &WalkOrder{Side: string(in.Side), Quantity: in.Quantity, RefPrice: in.RefPrice}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// WriteText writes the walkthrough for a terminal.
func (w RunWalk) writeText(out io.Writer) error {
	var err error
	printf := func(format string, args ...any) {
		if err == nil {
			_, err = fmt.Fprintf(out, format, args...)
		}
	}
	printf("\nWalkthrough: %s (%s)\n", w.Decider, w.RunID)
	if len(w.Instruments) == 0 {
		printf("  no decisions or fills\n")
	}
	var net float64
	for _, iw := range w.Instruments {
		net += iw.NetPnL
		held := "flat"
		if iw.Position != 0 {
			held = fmt.Sprintf("%+d @ %.2f", iw.Position, iw.AvgPrice)
		}
		printf("\n  %s: %d decisions (BUY %d, SELL %d, HOLD %d), %d fills, ends %s, net PnL %s\n",
			iw.Instrument, iw.Decisions, iw.Buys, iw.Sells, iw.Holds, iw.Fills, held, signed(iw.NetPnL))
		day := ""
		for _, r := range iw.Rows {
			if d := r.Time.In(ist).Format(time.DateOnly); d != day {
				day = d
				printf("    -- %s --\n", day)
			}
			printf("    %s  %s\n", r.Time.In(ist).Format(time.TimeOnly), r.describe())
		}
	}
	if len(w.Instruments) > 1 {
		printf("\n  all instruments: net PnL %s\n", signed(net))
	}
	return err
}

// describe is the row on one line.
func (r WalkRow) describe() string {
	state := fmt.Sprintf("pos %s  pnl %s", positionText(r.Position, r.AvgPrice), signed(r.PnL))
	switch r.Kind {
	case RowFill:
		return fmt.Sprintf("FILL   %s %d @ %.2f (costs %.2f) | %s", r.Side, r.Quantity, r.Price, r.Charges, state)
	case RowRiskExit:
		return fmt.Sprintf("EXIT   risk: %s @ %.2f | %s | %s", r.ExitReason, r.Price, gateText(r), state)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-4s p=%.2f", r.Action, r.Probability)
	if r.Size != "" {
		fmt.Fprintf(&b, " size %s", r.Size)
	}
	fmt.Fprintf(&b, " @ %.2f", r.Price)
	if r.HoldReason != "" {
		fmt.Fprintf(&b, " [decider failed: %s]", r.HoldReason)
	}
	if f := r.Features; f != nil {
		fmt.Fprintf(&b, " | ret5 %s ret15 %s session %s vwap %s vol %s range %s index %s",
			pct(f.Return5Pct), pct(f.Return15Pct), pct(&f.SessionReturnPct), pct(f.VWAPDistPct),
			ratio(f.VolumeRel, "x"), ratio(f.SessionRangePos, ""), pct(f.IndexSessionPct))
	}
	fmt.Fprintf(&b, " | %s", gateText(r))
	if o := r.Order; o != nil {
		fmt.Fprintf(&b, " -> order %s %d", o.Side, o.Quantity)
	}
	fmt.Fprintf(&b, " | %s", state)
	return "DECIDE " + b.String()
}

func gateText(r WalkRow) string {
	s := "gate " + r.Verdict
	if r.VerdictDetail != "" && r.Verdict != "approved" {
		s += " (" + r.VerdictDetail + ")"
	}
	return s
}

func positionText(qty int64, avg float64) string {
	if qty == 0 {
		return "flat"
	}
	return fmt.Sprintf("%+d @ %.2f", qty, avg)
}

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f%%", zeroSmall(*v))
}

func ratio(v *float64, unit string) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%s", *v, unit)
}

// signed formats v with an explicit sign, and never as "-0.00".
func signed(v float64) string { return fmt.Sprintf("%+.2f", zeroSmall(v)) }

// zeroSmall is 0 for a value that would print as 0.00 either way.
func zeroSmall(v float64) float64 {
	if math.Abs(v) < 0.005 {
		return 0
	}
	return v
}
