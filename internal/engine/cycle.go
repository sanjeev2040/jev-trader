package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

// result is a decider's answer, back on the engine loop.
type result struct {
	cycle *cycle
	d     domain.Decision
	err   error
}

// cycle is one decision in flight.
type cycle struct {
	id       string
	bar      domain.Bar
	alias    string
	snapshot []byte
}

var istLocation = risk.IST

// sessionOpen returns the NSE session open on the IST day of t.
func sessionOpen(t time.Time) time.Time {
	y, m, d := t.In(istLocation).Date()
	return time.Date(y, m, d, sessionOpenHour, sessionOpenMinute, 0, 0, istLocation)
}

// onBarClosed handles a closed bar: it goes to the sink, and a bar of the
// decision interval for a watched instrument starts a decision cycle.
func (e *Engine) onBarClosed(ctx context.Context, b domain.Bar) error {
	if e.cfg.BarSink != nil {
		e.cfg.BarSink(b)
	}
	if b.Interval != e.base {
		return nil
	}
	open := sessionOpen(b.Start)
	if b.Start.Before(open) {
		return nil // the pre-open session is not part of the trading day
	}
	token := b.Instrument.Token
	hist := e.history[token]
	if n := len(hist); n > 0 && hist[n-1].Start.In(istLocation).Format(time.DateOnly) != b.Start.In(istLocation).Format(time.DateOnly) {
		hist = nil // a new session day
	}
	e.history[token] = append(hist, b)

	if err := e.sampleEquity(ctx, b.End); err != nil {
		return err
	}
	if token == e.cfg.IndexToken {
		return nil
	}
	e.stats.Bars++
	return e.startDecision(ctx, b, open)
}

// sampleEquity logs the cumulative PnL after costs once per bar-close time.
//
// The risk book resets its PnL when its trading day rolls, which it does on
// the first Exits call of a new day; every path to a bar close makes that call
// first. So the book is on the day of the clock, not of a late-closing bar.
func (e *Engine) sampleEquity(ctx context.Context, at time.Time) error {
	if !at.After(e.lastEquity) {
		return nil
	}
	e.lastEquity = at
	day := e.clk.Now().In(istLocation).Format(time.DateOnly)
	if e.pnlDay != "" && day != e.pnlDay {
		e.carryPnL += e.dayPnL
		e.carryRealized += e.dayRealized
	}
	e.pnlDay = day
	e.dayPnL, e.dayRealized = e.book.PnL(), e.book.RealizedPnL()
	return e.cfg.Log.AppendEquity(ctx, e.cfg.RunID, decisionlog.EquityPoint{
		Time: at, PnL: e.carryPnL + e.dayPnL, Realized: e.carryRealized + e.dayRealized,
	})
}

// startDecision builds the snapshot for a closed bar and asks the decider on
// its own goroutine.
func (e *Engine) startDecision(ctx context.Context, b domain.Bar, open time.Time) error {
	token := b.Instrument.Token
	if e.pending[token] {
		e.stats.BusySkips++
		return nil
	}
	pos, _ := e.book.Position(token)
	alias := e.aliases.Alias(b.Instrument)
	snap, err := snapshot.Build(e.cfg.Snapshot, snapshot.Input{
		Alias:       alias,
		Now:         b.End,
		SessionOpen: open,
		Bars:        e.history[token],
		IndexBars:   e.indexBarsBy(b.End),
		Position:    pos,
	})
	if err == nil {
		var state []byte
		if state, err = snapshot.Encode(snap); err == nil {
			e.seq++
			c := &cycle{id: fmt.Sprintf("%s-c%06d", e.cfg.RunID, e.seq), bar: b, alias: alias, snapshot: state}
			e.pending[token] = true
			e.flight++
			go e.ask(ctx, c)
			return nil
		}
	}
	// A snapshot that cannot be built is a defect, not market data: say so
	// loudly and skip the bar rather than guess.
	e.stats.SnapshotErrors++
	e.log.Error("snapshot not built; bar skipped", "token", token, "bar_end", b.End, "err", err)
	return nil
}

// indexBarsBy returns the index's closed bars that ended by t.
func (e *Engine) indexBarsBy(t time.Time) []domain.Bar {
	if e.cfg.IndexToken == 0 {
		return nil
	}
	hist := e.history[e.cfg.IndexToken]
	n := len(hist)
	for n > 0 && hist[n-1].End.After(t) {
		n--
	}
	return hist[:n]
}

// ask runs the decider with its deadline and sends the answer to the loop.
func (e *Engine) ask(ctx context.Context, c *cycle) {
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return
	}
	dctx, cancel := context.WithTimeout(ctx, e.cfg.DecideTimeout)
	defer cancel()
	d, err := e.decider.Decide(dctx, c.snapshot)
	select {
	case e.results <- result{cycle: c, d: d, err: err}:
	case <-ctx.Done():
	}
}

// await blocks until no decision is in flight.
func (e *Engine) await(ctx context.Context) error {
	for e.flight > 0 {
		select {
		case r := <-e.results:
			if err := e.onResult(ctx, r); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// onResult judges a decider's answer, logs the cycle, and only then submits
// the order the risk gate approved.
func (e *Engine) onResult(ctx context.Context, r result) error {
	c := r.cycle
	token := c.bar.Instrument.Token
	delete(e.pending, token)
	e.flight--

	d := r.d
	if r.err != nil { // HoldOnFailure never errs; this keeps the guarantee local
		d = domain.Decision{Action: domain.ActionHold, HoldReason: domain.ReasonError, Probabilities: map[domain.Action]float64{}}
	}
	if e.cfg.Latency > 0 {
		d.Latency = e.cfg.Latency
	}
	// The answer applies at bar close plus latency. A simulated clock jumps
	// there; the wall clock is already past it.
	e.moveClock(c.bar.End.Add(max(d.Latency, 0)))
	now := e.clk.Now()

	verdict := e.gate.Evaluate(risk.Request{CycleID: c.id, Instrument: c.bar.Instrument, Decision: d}, e.book, now)
	row := decisionlog.Cycle{
		CycleID: c.id, RunID: e.cfg.RunID, Source: decisionlog.SourceDecision, Mode: e.cfg.Mode,
		Instrument: c.bar.Instrument, Alias: c.alias, BarClose: c.bar.End, Price: c.bar.Close, AppliedAt: now,
		SchemaVersion: snapshot.SchemaVersion, Snapshot: c.snapshot, Decision: &d,
		Verdict: string(verdict.Reason), VerdictDetail: verdict.Detail,
	}
	approval, approved := verdict.Approval()
	if approved {
		in := approval.Intent()
		row.Intent, row.ClientID = &in, orders.ClientID(in.CycleID)
	}
	if err := e.cfg.Log.AppendCycle(ctx, row); err != nil {
		return err
	}
	e.stats.Cycles++
	if d.Action == domain.ActionHold {
		e.stats.Holds++
		if d.HoldReason != "" {
			e.stats.HoldSubstitutions++
		}
	}
	if !approved {
		return nil
	}
	e.stats.Approved++
	return e.submit(ctx, approval, c.id)
}
