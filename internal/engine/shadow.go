package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// Shadow configures the shadow log (issue #60): every candidate signal of a
// run is written to the decision log whether or not it is traded, so what the
// market did after it can be read off the recorded bars later, independent of
// the capital budget that decides what the bot can act on.
type Shadow struct {
	// Picks are the day's picker entries. They are logged on the first
	// decision bar of each day, counted from PickTime.
	Picks []ShadowPick
	// PickTime is when the picks count from, as an offset from IST midnight:
	// the first moment the risk gate lets an entry through.
	PickTime time.Duration
	// WindowEnd is the end of the trading window, as an offset from IST
	// midnight; zero for none. It is the "window end" horizon of every signal.
	WindowEnd time.Duration
}

// ShadowPick is one picker entry and what the picker made of it.
type ShadowPick struct {
	Instrument domain.Instrument
	// Detail is the entry's rank, score, signals and news scores as JSON; the
	// log keeps it as it is.
	Detail json.RawMessage
}

// Shadow signal labels. A decision's label is its action; a substituted HOLD
// (the decider failed or timed out) is not an answer and has its own.
const (
	shadowPickSignal     = "pick"
	shadowDecisionPrefix = "decision:"
	shadowDecisionFailed = shadowDecisionPrefix + "FAILED"
	shadowDirectionRise  = 1
	shadowDirectionFall  = -1
)

func (e *Engine) shadowDayOffset(t time.Time, offset time.Duration) time.Time {
	if offset <= 0 {
		return time.Time{}
	}
	y, m, d := t.In(istLocation).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, istLocation).Add(offset)
}

// shadowPicks logs the picker entries on the first decision bar of each trading
// day: a restart the same day logs them again and the log keeps the first.
func (e *Engine) shadowPicks(ctx context.Context, barStart time.Time) {
	sh := e.cfg.Shadow
	if sh == nil || len(sh.Picks) == 0 {
		return
	}
	day := barStart.In(istLocation).Format(time.DateOnly)
	if day == e.shadowPickDay {
		return
	}
	e.shadowPickDay = day
	ref := e.shadowDayOffset(barStart, sh.PickTime)
	if ref.IsZero() {
		ref = barStart
	}
	for _, p := range sh.Picks {
		e.shadowAppend(ctx, decisionlog.ShadowSignal{
			RunID: e.cfg.RunID, Day: day, Kind: decisionlog.ShadowPick, Signal: shadowPickSignal,
			Instrument: p.Instrument, RefTime: ref, WindowEnd: e.shadowDayOffset(barStart, sh.WindowEnd),
			IndexToken: e.cfg.IndexToken, Direction: shadowDirectionRise, Detail: p.Detail,
		})
	}
}

// shadowDecision logs a decider's answer with the gate's verdict on it.
func (e *Engine) shadowDecision(ctx context.Context, c *cycle, d domain.Decision, reason risk.Reason, detail string, approved bool) {
	sh := e.cfg.Shadow
	if sh == nil {
		return
	}
	signal, dir := shadowDecisionPrefix+string(d.Action), shadowDirectionRise
	switch {
	case d.Action == domain.ActionHold && d.HoldReason != "":
		signal = shadowDecisionFailed
	case d.Action == domain.ActionSell:
		dir = shadowDirectionFall
	}
	e.shadowAppend(ctx, decisionlog.ShadowSignal{
		RunID: e.cfg.RunID, Day: c.bar.End.In(istLocation).Format(time.DateOnly), Kind: decisionlog.ShadowDecision, Signal: signal,
		Instrument: c.bar.Instrument, RefTime: c.bar.End, RefPrice: c.bar.Close,
		WindowEnd: e.shadowDayOffset(c.bar.End, sh.WindowEnd), IndexToken: e.cfg.IndexToken, Direction: dir,
		CycleID: c.id, Action: string(d.Action), Probability: d.Probability,
		Verdict: string(reason), VerdictDetail: detail, Traded: approved,
	})
}

// shadowAppend writes a signal. The shadow log is evidence, not part of the
// trading path: a failure is counted and logged and the run goes on.
func (e *Engine) shadowAppend(ctx context.Context, s decisionlog.ShadowSignal) {
	if _, err := e.cfg.Log.AppendShadowSignal(ctx, s); err != nil {
		e.stats.ShadowErrors++
		e.log.Error("shadow signal not logged", "signal", s.Signal, "instrument", s.Instrument.Symbol, "err", err)
	}
}
