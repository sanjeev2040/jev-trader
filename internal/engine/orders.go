package engine

import (
	"context"
	"errors"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// submit places an approved order. Its cycle row is already in the log.
func (e *Engine) submit(ctx context.Context, a risk.Approval, cycleID string) error {
	intent := a.Intent()
	_, err := e.mgr.Submit(ctx, a)
	if err != nil {
		e.stats.PlaceFailures++
		e.log.Error("order not placed", "cycle", cycleID, "err", err)
		e.book.Settle(intent.Instrument.Token)
		e.gate.NoteRejection(e.book)
		return e.cfg.Log.AppendOrderEvent(ctx, decisionlog.OrderEvent{
			RunID: e.cfg.RunID, CycleID: cycleID, ClientID: orders.ClientID(cycleID),
			Instrument: intent.Instrument, Side: intent.Side,
			Status: decisionlog.StatusPlaceFailed, Reason: err.Error(), Time: e.clk.Now(),
		})
	}
	e.stats.OrdersPlaced++
	return e.drain(ctx)
}

// drain applies the broker updates that are ready.
func (e *Engine) drain(ctx context.Context) error {
	for {
		select {
		case u := <-e.updates:
			if err := e.onUpdate(ctx, u); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// onUpdate folds a broker update into the order manager and the risk book and
// logs it.
func (e *Engine) onUpdate(ctx context.Context, u domain.OrderUpdate) error {
	o, fill, err := e.mgr.Apply(u)
	switch {
	case errors.Is(err, orders.ErrUnknownOrder), errors.Is(err, orders.ErrBrokerIDMismatch):
		e.stats.UnknownUpdates++
		e.log.Warn("broker update ignored", "client_id", u.ClientID, "broker_id", u.BrokerID, "err", err)
		return nil
	case err != nil: // an overfill still carries its fill: book it, and say so
		e.log.Error("broker update inconsistent", "client_id", u.ClientID, "err", err)
	}

	if fill != nil {
		e.book.ApplyFill(*fill)
		e.stats.Fills++
	}
	token := o.Intent.Instrument.Token
	switch u.Status {
	case domain.OrderStatusOpen:
		e.book.NoteAccepted()
	case domain.OrderStatusRejected:
		e.book.Settle(token)
		e.gate.NoteRejection(e.book)
	case domain.OrderStatusFilled, domain.OrderStatusCancelled:
		e.book.Settle(token)
	}
	return e.cfg.Log.AppendOrderEvent(ctx, decisionlog.OrderEvent{
		RunID: e.cfg.RunID, CycleID: o.Intent.CycleID, ClientID: orders.ClientID(o.Intent.CycleID), BrokerID: u.BrokerID,
		Instrument: o.Intent.Instrument, Side: o.Intent.Side, Status: string(u.Status),
		FilledQuantity: o.FilledQuantity, AveragePrice: o.AveragePrice, Costs: o.Costs,
		Reason: u.Reason, Time: u.Time, Fill: fill,
	})
}

// exits asks the risk gate for the exits due at now, cancels the entries it
// names and submits the flattening orders. They do not wait for the decider
// and each is logged as a cycle of its own.
func (e *Engine) exits(ctx context.Context, now time.Time) error {
	plan := e.gate.Exits(e.book, now)
	if plan.Reason == "" {
		return nil
	}
	for _, in := range plan.Cancel {
		err := e.mgr.Cancel(ctx, orders.ClientID(in.CycleID))
		if err != nil && !errors.Is(err, orders.ErrOrderClosed) && !errors.Is(err, orders.ErrNotAcknowledged) {
			e.log.Error("cancel failed", "cycle", in.CycleID, "err", err)
		}
	}
	if len(plan.Cancel) > 0 {
		if err := e.drain(ctx); err != nil {
			return err
		}
	}
	for _, a := range plan.Approvals {
		in := a.Intent()
		row := decisionlog.Cycle{
			CycleID: in.CycleID, RunID: e.cfg.RunID, Source: decisionlog.SourceRisk, Mode: e.cfg.Mode,
			Instrument: in.Instrument, Alias: e.aliases.Alias(in.Instrument), BarClose: now, Price: in.RefPrice, AppliedAt: now,
			ExitReason: string(a.Reason()), Verdict: string(a.Reason()),
			Intent: &in, ClientID: orders.ClientID(in.CycleID),
		}
		if err := e.cfg.Log.AppendCycle(ctx, row); err != nil {
			return err
		}
		e.stats.Cycles++
		e.stats.Exits++
		if err := e.submit(ctx, a, in.CycleID); err != nil {
			return err
		}
	}
	return nil
}
