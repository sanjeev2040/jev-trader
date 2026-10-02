// Package risk is the deterministic risk gate. It has final authority over
// every order: only this package can mint the Approval that the order manager
// requires, and it emits the risk-driven exits (square-off, daily loss limit,
// kill switch) without waiting for a decider.
package risk

import (
	"fmt"
	"maps"
	"math"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Reason is a machine-readable code for a verdict, an exit or a kill.
type Reason string

// Verdict reasons, in the order the gate checks them.
const (
	ReasonApproved         Reason = "approved"
	ReasonHold             Reason = "hold"             // the decider chose HOLD; no order is warranted
	ReasonInvalidDecision  Reason = "invalid_decision" // unknown action or size, or probability outside [0, 1]
	ReasonKillSwitch       Reason = "kill_switch"
	ReasonOutsideSession   Reason = "outside_session"
	ReasonDailyLoss        Reason = "daily_loss_limit"
	ReasonBelowThreshold   Reason = "below_probability_threshold"
	ReasonOrderInFlight    Reason = "order_in_flight"
	ReasonShortNotAllowed  Reason = "short_not_allowed"
	ReasonNoPrice          Reason = "no_price"
	ReasonSizeTooSmall     Reason = "size_too_small"
	ReasonPositionCap      Reason = "position_cap"
	ReasonGrossCap         Reason = "gross_cap"
	ReasonOrderRate        Reason = "order_rate_limit"
	ReasonDailyOrderBudget Reason = "daily_order_budget"
)

// Exit reasons that are not also verdict reasons.
const (
	ReasonSquareOff Reason = "square_off"
)

// Kill-switch reasons.
const (
	ReasonKillSignal       Reason = "kill_signal"       // SIGUSR1
	ReasonKillFile         Reason = "kill_file"         // a KILL file in the state directory
	ReasonBrokerRejections Reason = "broker_rejections" // too many consecutive broker rejections
	ReasonIPWhitelist      Reason = "ip_whitelist"      // the broker refused the order IP
)

// Request is one decision for the gate to judge.
type Request struct {
	CycleID    string // decision cycle id, carried into the order intent
	Instrument domain.Instrument
	Decision   domain.Decision
}

// Verdict is the gate's answer to a Request.
type Verdict struct {
	Reason   Reason // ReasonApproved, or the first check that refused
	Detail   string // human-readable context for the decision log
	approval Approval
}

// Approved reports whether the verdict carries an Approval.
func (v Verdict) Approved() bool { return v.approval.Valid() }

// Approval returns the approval, if the order was approved.
func (v Verdict) Approval() (Approval, bool) { return v.approval, v.approval.Valid() }

// ExitPlan is the set of risk-driven exits due now.
type ExitPlan struct {
	Reason    Reason     // why the book is being flattened; empty if no exit is due
	Approvals []Approval // reduce-only orders that flatten open positions
	// Cancel lists in-flight entry orders. The order manager should cancel
	// them; an instrument with an order in flight gets no exit until that
	// order settles.
	Cancel []domain.OrderIntent
}

// Gate applies Limits to decisions and to the book. Apart from the Book it
// is given, it has no state and does no I/O: the same inputs always produce
// the same verdict, so replay and live trading judge alike.
type Gate struct {
	limits Limits
}

// NewGate validates limits and returns a gate that enforces them.
func NewGate(limits Limits) (*Gate, error) {
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("risk limits: %w", err)
	}
	limits.MinProbability = maps.Clone(limits.MinProbability)
	limits.SizeNotional = maps.Clone(limits.SizeNotional)
	limits.Location = limits.location()
	return &Gate{limits: limits}, nil
}

// Limits returns a copy of the gate's limits.
func (g *Gate) Limits() Limits {
	l := g.limits
	l.MinProbability = maps.Clone(l.MinProbability)
	l.SizeNotional = maps.Clone(l.SizeNotional)
	return l
}

// Evaluate judges one decision against the book at time now. Checks run in
// a fixed order and the first that fails decides the verdict:
//
//  1. kill switch;
//  2. session window (new entries only);
//  3. daily loss limit (new entries only);
//  4. probability threshold for the chosen action;
//  5. one order in flight per instrument, shorts, and position caps;
//  6. order-rate limits and the daily order budget.
//
// A BUY against a short or a SELL against a long closes the whole position
// and never reverses it; it is exempt from the session window, the loss
// limit and the caps. An approval is recorded in the book as an order in
// flight and against the rate limits.
func (g *Gate) Evaluate(req Request, b *Book, now time.Time) Verdict {
	l := g.limits
	now = now.In(l.Location)
	b.rollDay(now)
	b.pruneOrders(now)
	g.checkLoss(b)

	d := req.Decision
	switch d.Action {
	case domain.ActionHold:
		return Verdict{Reason: ReasonHold, Detail: d.HoldReason}
	case domain.ActionBuy, domain.ActionSell:
	default:
		return refuse(ReasonInvalidDecision, "unknown action %q", d.Action)
	}
	if math.IsNaN(d.Probability) || d.Probability < 0 || d.Probability > 1 {
		return refuse(ReasonInvalidDecision, "probability %v outside [0, 1]", d.Probability)
	}

	token := req.Instrument.Token
	pos := b.positions[token].Quantity
	reduce := (d.Action == domain.ActionSell && pos > 0) || (d.Action == domain.ActionBuy && pos < 0)

	// 1. Kill switch. Gate.Exits flattens the book while it is tripped.
	if b.killed {
		return refuse(ReasonKillSwitch, "kill switch tripped: %s", b.killReason)
	}
	// 2. Session window.
	if !reduce && !g.inEntryWindow(now) {
		return refuse(ReasonOutsideSession, "entries allowed %s–%s %s", l.EntryStart, l.EntryEnd, l.Location)
	}
	// 3. Daily loss limit.
	if !reduce && b.lossHalted {
		return refuse(ReasonDailyLoss, "day PnL %.2f breached limit -%.2f", b.PnL(), l.MaxDailyLoss)
	}
	// 4. Probability threshold.
	if minP := l.MinProbability[d.Action]; d.Probability < minP {
		return refuse(ReasonBelowThreshold, "%s probability %.4f below %.4f", d.Action, d.Probability, minP)
	}
	// 5. Position checks.
	if b.InFlight(token) {
		return refuse(ReasonOrderInFlight, "an order for token %d is still open", token)
	}
	var qty int64
	price := b.marks[token]
	if reduce {
		qty = abs(pos)
	} else {
		if d.Action == domain.ActionSell && !l.AllowShort {
			return refuse(ReasonShortNotAllowed, "SELL with no long position and shorts disabled")
		}
		notional, ok := l.SizeNotional[d.Size]
		if !ok {
			return refuse(ReasonInvalidDecision, "unknown size bucket %q", d.Size)
		}
		if !(price > 0) {
			return refuse(ReasonNoPrice, "no last price for token %d", token)
		}
		qty = int64(math.Floor(notional / price))
		if qty < 1 {
			return refuse(ReasonSizeTooSmall, "size %s (%.2f) buys no shares at %.2f", d.Size, notional, price)
		}
		if n := float64(abs(pos)+qty) * price; n > l.MaxPositionNotional {
			return refuse(ReasonPositionCap, "position notional %.2f over cap %.2f", n, l.MaxPositionNotional)
		}
		if n := b.grossExposure() + float64(qty)*price; n > l.MaxGrossNotional {
			return refuse(ReasonGrossCap, "gross notional %.2f over cap %.2f", n, l.MaxGrossNotional)
		}
	}
	// 6. Order rate and daily budget.
	if reason, detail := g.rateLimited(b, now); reason != "" {
		return refuse(reason, "%s", detail)
	}
	if b.dayOrders >= l.MaxOrdersPerDay {
		return refuse(ReasonDailyOrderBudget, "%d orders today, budget %d", b.dayOrders, l.MaxOrdersPerDay)
	}

	if price == 0 {
		price = b.price(token)
	}
	intent := domain.OrderIntent{
		CycleID:    req.CycleID,
		Instrument: req.Instrument,
		Side:       d.Action,
		Quantity:   qty,
		RefPrice:   price,
		ReduceOnly: reduce,
	}
	b.record(intent, now)
	b.dayOrders++
	return Verdict{Reason: ReasonApproved, approval: newApproval(intent, SourceDecision, ReasonApproved, now)}
}

// Exits returns the risk-driven exits due at time now. While the kill switch
// is tripped, after the daily loss limit is breached, or at and after the
// square-off time, every open position is closed with a reduce-only order.
// Exits do not wait for a decider and do not count against the daily order
// budget, but they do respect the per-second and per-minute order limits:
// call Exits again on later events until the book is flat.
func (g *Gate) Exits(b *Book, now time.Time) ExitPlan {
	l := g.limits
	now = now.In(l.Location)
	b.rollDay(now)
	b.pruneOrders(now)
	g.checkLoss(b)

	var plan ExitPlan
	switch {
	case b.killed:
		plan.Reason = ReasonKillSwitch
	case b.lossHalted:
		plan.Reason = ReasonDailyLoss
	case !now.Before(l.SquareOff.On(now)):
		plan.Reason = ReasonSquareOff
	default:
		return plan
	}

	for _, in := range b.inflightOrders() {
		if !in.ReduceOnly {
			plan.Cancel = append(plan.Cancel, in)
		}
	}
	for _, p := range b.Positions() {
		token := p.Instrument.Token
		if b.InFlight(token) {
			continue
		}
		if reason, _ := g.rateLimited(b, now); reason != "" {
			break
		}
		side := domain.ActionSell
		if p.Quantity < 0 {
			side = domain.ActionBuy
		}
		intent := domain.OrderIntent{
			CycleID:    fmt.Sprintf("risk-%s-%d-%d", plan.Reason, token, now.UnixNano()),
			Instrument: p.Instrument,
			Side:       side,
			Quantity:   abs(p.Quantity),
			RefPrice:   b.price(token),
			ReduceOnly: true,
		}
		b.record(intent, now)
		plan.Approvals = append(plan.Approvals, newApproval(intent, SourceRisk, plan.Reason, now))
	}
	return plan
}

// NoteRejection counts a broker rejection and trips the kill switch after
// MaxConsecutiveRejections in a row. Book.NoteAccepted resets the count.
func (g *Gate) NoteRejection(b *Book) {
	b.rejections++
	if b.rejections >= g.limits.MaxConsecutiveRejections {
		b.Kill(ReasonBrokerRejections)
	}
}

// NextSquareOff returns the first square-off time at or after now.
func (g *Gate) NextSquareOff(now time.Time) time.Time {
	now = now.In(g.limits.Location)
	t := g.limits.SquareOff.On(now)
	if t.Before(now) {
		t = g.limits.SquareOff.On(now.AddDate(0, 0, 1))
	}
	return t
}

func (g *Gate) inEntryWindow(now time.Time) bool {
	return !now.Before(g.limits.EntryStart.On(now)) && now.Before(g.limits.EntryEnd.On(now))
}

// checkLoss latches the loss halt for the rest of the day once the day's PnL
// reaches -MaxDailyLoss.
func (g *Gate) checkLoss(b *Book) {
	if !b.lossHalted && b.PnL() <= -g.limits.MaxDailyLoss {
		b.lossHalted = true
	}
}

func (g *Gate) rateLimited(b *Book, now time.Time) (Reason, string) {
	if n := b.ordersAfter(now.Add(-time.Second)); n >= g.limits.MaxOrdersPerSecond {
		return ReasonOrderRate, fmt.Sprintf("%d orders in the last second, limit %d", n, g.limits.MaxOrdersPerSecond)
	}
	if n := len(b.orderTimes); n >= g.limits.MaxOrdersPerMinute {
		return ReasonOrderRate, fmt.Sprintf("%d orders in the last minute, limit %d", n, g.limits.MaxOrdersPerMinute)
	}
	return "", ""
}

func refuse(reason Reason, format string, args ...any) Verdict {
	return Verdict{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
