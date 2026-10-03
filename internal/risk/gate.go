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
	ReasonPositionCap      Reason = "position_cap"
	ReasonGrossCap         Reason = "gross_cap"
	ReasonBudgetFull       Reason = "budget_full"      // not one share fits in the capital that is free
	ReasonForeignPosition  Reason = "foreign_position" // the instrument holds something the bot did not open
	ReasonRotation         Reason = "rotation"         // the budget is full: an open position is being sold to make room
	ReasonOrderRate        Reason = "order_rate_limit"
	ReasonDailyOrderBudget Reason = "daily_order_budget"
	ReasonMinHold          Reason = "min_hold"         // a position is being sold before its minimum holding time
	ReasonCooldown         Reason = "reentry_cooldown" // the instrument was sold out of too recently to buy again
	ReasonCostEdge         Reason = "cost_edge"        // the expected move does not beat the round-trip costs plus margin
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
	// Rotated says capital was freed for this entry by a rotation, so the
	// gate must not rotate again if the entry still does not fit.
	Rotated bool
}

// Verdict is the gate's answer to a Request.
type Verdict struct {
	Reason   Reason // ReasonApproved, ReasonRotation, or the first check that refused
	Detail   string // human-readable context for the decision log
	approval Approval
	rotation Approval
}

// Approved reports whether the verdict carries an Approval.
func (v Verdict) Approved() bool { return v.approval.Valid() }

// Approval returns the approval, if the order was approved.
func (v Verdict) Approval() (Approval, bool) { return v.approval, v.approval.Valid() }

// Rotation returns the approval to sell one of the bot's own positions, if the
// budget was full and the gate chose to rotate. The entry itself is not
// approved yet: the capital is only free once the sale fills, and the caller
// then asks the gate again with Request.Rotated set.
func (v Verdict) Rotation() (Approval, bool) { return v.rotation, v.rotation.Valid() }

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
	limits.SizeFraction = maps.Clone(limits.SizeFraction)
	limits.Location = limits.location()
	return &Gate{limits: limits}, nil
}

// Limits returns a copy of the gate's limits.
func (g *Gate) Limits() Limits {
	l := g.limits
	l.MinProbability = maps.Clone(l.MinProbability)
	l.SizeFraction = maps.Clone(l.SizeFraction)
	return l
}

// Evaluate judges one decision against the book at time now. Checks run in
// a fixed order and the first that fails decides the verdict:
//
//  1. kill switch;
//  2. session window (new entries only);
//  3. daily loss limit (new entries only);
//  4. probability threshold for the chosen action;
//  5. one order in flight per instrument; an exit before the position's
//     minimum holding time; an entry within the re-entry cooldown, shorts,
//     the cost-aware check, the position cap and the capital budget;
//  6. order-rate limits and the daily order budget.
//
// An instrument holding something the bot did not open is refused outright,
// before any other check: the bot only ever trades its own positions.
//
// An entry is sized from the capital budget: the size bucket's share of the
// budget, at least one share, clipped to the capital that is free. If not even
// one share fits, the gate may sell one of the bot's own positions to make
// room (see Verdict.Rotation); otherwise the entry is refused.
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
	if b.IsForeign(token) {
		return refuse(ReasonForeignPosition, "token %d holds a position the bot did not open and is never traded", token)
	}
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
	var sized string
	price := b.marks[token]
	if reduce {
		if e, ok := b.entries[token]; ok && l.MinHold > 0 && now.Sub(e.at) < l.MinHold {
			return refuse(ReasonMinHold, "held %s of the minimum %s since the entry at %s; square-off, the loss limit and the kill switch still exit at once",
				now.Sub(e.at).Round(time.Second), l.MinHold, e.at.In(l.Location).Format("15:04:05"))
		}
		qty = abs(pos)
	} else {
		if last, ok := b.exits[token]; ok && l.ReentryCooldown > 0 && now.Sub(last) < l.ReentryCooldown {
			return refuse(ReasonCooldown, "sold out of token %d at %s, %s ago; re-entry allowed after %s",
				token, last.In(l.Location).Format("15:04:05"), now.Sub(last).Round(time.Second), l.ReentryCooldown)
		}
		if d.Action == domain.ActionSell && !l.AllowShort {
			return refuse(ReasonShortNotAllowed, "SELL with no long position and shorts disabled")
		}
		target, ok := l.EntryNotional(d.Size)
		if !ok {
			return refuse(ReasonInvalidDecision, "unknown size bucket %q", d.Size)
		}
		if !(price > 0) {
			return refuse(ReasonNoPrice, "no last price for token %d", token)
		}
		if l.CostAware {
			if v, ok := g.costEdge(b, token, price, d.Probability, now); !ok {
				return v
			}
		}
		// At least one share, then clipped to the position cap and the free capital.
		qty = max(int64(math.Floor(target/price)), 1)
		held := float64(abs(pos)) * price
		if room := int64(math.Floor((l.PositionCap() - held) / price)); room < qty {
			if room < 1 {
				return refuse(ReasonPositionCap, "position notional %.2f plus one share at %.2f over cap %.2f", held, price, l.PositionCap())
			}
			qty = room
		}
		free := l.Budget() - b.Committed()
		if fit := int64(math.Floor(free / price)); fit < qty {
			if fit < 1 {
				if l.MaxGrossNotional < l.CapitalBudget/(1+l.CostReserveRate) {
					return refuse(ReasonGrossCap, "committed %.2f of hard gross cap %.2f: one share at %.2f does not fit", b.Committed(), l.MaxGrossNotional, price)
				}
				if !req.Rotated {
					if v, ok := g.rotate(req, b, price, now); ok {
						return v
					}
				}
				return refuse(ReasonBudgetFull, "committed %.2f of budget %.2f (%.2f after estimated costs): one share at %.2f does not fit", b.Committed(), l.CapitalBudget, l.Budget(), price)
			}
			qty = fit
			sized = fmt.Sprintf("sized to free capital %.2f of budget %.2f", free, l.CapitalBudget)
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
	if reduce {
		b.noteExit(token, now)
	} else {
		b.noteEntry(token, d.Probability, now)
	}
	return Verdict{Reason: ReasonApproved, Detail: sized, approval: newApproval(intent, SourceDecision, ReasonApproved, now)}
}

// costEdge checks that an entry's expected move beats the estimated round-trip
// costs plus the margin. It returns the refusal and false if it does not.
func (g *Gate) costEdge(b *Book, token uint32, price, p float64, now time.Time) (Verdict, bool) {
	l := g.limits
	r, ok := b.recentRange(token, now, l.RangeWindow)
	if !ok {
		return Verdict{}, true // no price history to judge by
	}
	move := (2*p - 1) * r
	need := price * (l.CostReserveRate + l.CostMargin)
	if move >= need {
		return Verdict{}, true
	}
	return refuse(ReasonCostEdge, "expected move %.2f (2×%.2f−1 of a %.2f range over %s) is below round-trip costs and margin %.2f (%.2f%% of %.2f)",
		move, p, r, l.RangeWindow, need, 100*(l.CostReserveRate+l.CostMargin), price), false
}

// rotate sells the bot's weakest eligible position to make room for the entry
// in req, whose one share does not fit in the free capital. A position is
// eligible only if the bot opened it today, it has been held for
// RotationMinHold, it has no order in flight, the entry's probability beats
// the probability it was opened at by RotationMargin, and selling it frees
// enough for one share. It checks the same limits as any other order; it
// returns false when nothing qualifies.
func (g *Gate) rotate(req Request, b *Book, price float64, now time.Time) (Verdict, bool) {
	l := g.limits
	d := req.Decision
	need := price - (l.Budget() - b.Committed())
	var victim domain.Position
	var weakest float64
	var found bool
	for _, p := range b.Positions() { // by token, so ties break the same way every run
		token := p.Instrument.Token
		e, ok := b.entries[token]
		switch {
		case !ok, p.Quantity <= 0, b.InFlight(token), b.IsForeign(token), token == req.Instrument.Token:
			continue // not provably opened by the bot today, or not a plain long
		case e.at.In(l.Location).Format(time.DateOnly) != now.Format(time.DateOnly), now.Sub(e.at) < l.RotationMinHold:
			continue
		case d.Probability < e.probability+l.RotationMargin:
			continue
		case float64(p.Quantity)*p.AvgPrice < need:
			continue
		}
		if !found || e.probability < weakest {
			victim, weakest, found = p, e.probability, true
		}
	}
	if !found {
		return Verdict{}, false
	}
	if reason, _ := g.rateLimited(b, now); reason != "" {
		return Verdict{}, false
	}
	if b.dayOrders >= l.MaxOrdersPerDay {
		return Verdict{}, false
	}
	token := victim.Instrument.Token
	intent := domain.OrderIntent{
		CycleID:    fmt.Sprintf("%s-rot-%d", req.CycleID, token),
		Instrument: victim.Instrument,
		Side:       domain.ActionSell,
		Quantity:   victim.Quantity,
		RefPrice:   b.price(token),
		ReduceOnly: true,
	}
	b.record(intent, now)
	b.dayOrders++
	b.noteExit(token, now)
	detail := fmt.Sprintf("budget %.2f is full: selling %s (opened at probability %.2f) to make room for %s at probability %.2f",
		l.CapitalBudget, victim.Instrument.Symbol, weakest, req.Instrument.Symbol, d.Probability)
	return Verdict{Reason: ReasonRotation, Detail: detail, rotation: newApproval(intent, SourceRotation, ReasonRotation, now)}, true
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
		b.noteExit(token, now)
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
