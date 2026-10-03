package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// rotationWindow is how long a deferred entry waits for the sale that frees
// its capital. A stronger BUY that cannot be acted on within it is stale.
const rotationWindow = 2 * time.Minute

// Verdicts the engine itself gives a deferred entry.
const (
	verdictRotationAbandoned = "rotation_abandoned"
)

// rotation is a BUY waiting for the sale of one of the bot's own positions to
// fill, which frees the capital it needs. The gate does not approve the entry
// until the capital is free: the engine asks it again once the sale has filled.
type rotation struct {
	cycle    *cycle
	decision domain.Decision
	at       time.Time
}

// startRotation logs and submits the sale the gate chose to make room for the
// entry of cycle c, and remembers the entry. The decision's own cycle row is
// already in the log.
func (e *Engine) startRotation(ctx context.Context, c *cycle, d domain.Decision, sale risk.Approval, now time.Time) error {
	in := sale.Intent()
	token := in.Instrument.Token
	row := decisionlog.Cycle{
		CycleID: in.CycleID, RunID: e.cfg.RunID, Source: decisionlog.SourceRisk, Mode: e.cfg.Mode,
		Instrument: in.Instrument, Alias: e.aliases.Alias(in.Instrument), BarClose: now, Price: in.RefPrice, AppliedAt: now,
		ExitReason: string(sale.Reason()), Verdict: string(sale.Reason()),
		VerdictDetail: fmt.Sprintf("makes room for %s (cycle %s)", c.bar.Instrument.Symbol, c.id),
		Intent:        &in, ClientID: orders.ClientID(in.CycleID),
	}
	if err := e.cfg.Log.AppendCycle(ctx, row); err != nil {
		return err
	}
	e.stats.Cycles++
	e.stats.Rotations++
	e.rotations[token] = &rotation{cycle: c, decision: d, at: now}
	return e.submit(ctx, sale, in.CycleID)
}

// resumeRotation runs when an order in token reaches a terminal state. If it
// was a rotation sale and the position is gone, the capital is free and the
// entry it was for goes back to the gate; otherwise the entry is abandoned.
func (e *Engine) resumeRotation(ctx context.Context, token uint32) error {
	r, ok := e.rotations[token]
	if !ok {
		return nil
	}
	delete(e.rotations, token)
	now := e.clk.Now()
	c := r.cycle
	instrument := c.bar.Instrument
	id := c.id + "-rot-buy"

	row := decisionlog.Cycle{
		CycleID: id, RunID: e.cfg.RunID, Source: decisionlog.SourceDecision, Mode: e.cfg.Mode,
		Instrument: instrument, Alias: c.alias, BarClose: c.bar.End, Price: c.bar.Close, AppliedAt: now,
		Decision: &r.decision,
	}
	var abandoned string
	switch _, held := e.book.Position(token); {
	case held:
		abandoned = "the sale did not complete, so no capital was freed"
	case now.Sub(r.at) > rotationWindow:
		abandoned = fmt.Sprintf("the sale took longer than %s", rotationWindow)
	case e.holding(instrument.Token):
		abandoned = "the instrument was entered in the meantime"
	}
	if abandoned != "" {
		row.Verdict, row.VerdictDetail = verdictRotationAbandoned, abandoned
		e.stats.Cycles++
		return e.cfg.Log.AppendCycle(ctx, row)
	}

	verdict := e.gate.Evaluate(risk.Request{CycleID: id, Instrument: instrument, Decision: r.decision, Rotated: true}, e.book, now)
	row.Verdict, row.VerdictDetail = string(verdict.Reason), verdict.Detail
	approval, approved := verdict.Approval()
	if approved {
		in := approval.Intent()
		row.Intent, row.ClientID = &in, orders.ClientID(in.CycleID)
	}
	if err := e.cfg.Log.AppendCycle(ctx, row); err != nil {
		return err
	}
	e.stats.Cycles++
	if !approved {
		return nil
	}
	e.stats.Approved++
	return e.submit(ctx, approval, id)
}

// holding reports whether the instrument has a position or an order in flight.
func (e *Engine) holding(token uint32) bool {
	_, open := e.book.Position(token)
	return open || e.book.InFlight(token)
}
