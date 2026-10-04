package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// stopLimits are the trade-for-hours limits with a 2% stop: an hour's minimum
// hold and cooldown that would keep a decider in a falling stock, which is
// what the stop is for. The entry checks are off so a script's BUY goes in.
func stopLimits(edit func(*risk.Limits)) func(*risk.Limits) {
	return func(l *risk.Limits) {
		def := risk.DefaultLimits()
		l.MinHold, l.ReentryCooldown = def.MinHold, def.ReentryCooldown
		l.StopLossPct = 0.02
		if edit != nil {
			edit(l)
		}
	}
}

// gapTicks is one tick a minute from 09:15 at 1,000 until the minute gapAt,
// then at 940: a 6% gap through a 2% stop between two ticks.
func gapTicks(in domain.Instrument, n, gapAt int) []marketdata.Event {
	var evs []marketdata.Event
	for i := range n {
		p := 1000.0
		if i >= gapAt {
			p = 940
		}
		t := tickAt(in, 9, 15+i, 30, p)
		evs = append(evs, marketdata.Event{Tick: &t})
	}
	return evs
}

func riskExits(cs []decisionlog.Cycle) []decisionlog.Cycle {
	var out []decisionlog.Cycle
	for _, c := range cs {
		if c.Source == decisionlog.SourceRisk {
			out = append(out, c)
		}
	}
	return out
}

// A stock gaps 6% through the stop eleven minutes in, with the decider locked
// out by the hour's minimum hold. The gate sells on the very tick that shows
// the gap, at the gapped price, and the decider's own SELL before and after is
// refused by min_hold or has nothing left to sell.
func TestStopSellsOnTheGapTickWhileMinHoldBlocksTheDecider(t *testing.T) {
	d := scripted{"I01": {
		6:  buyAt(0.9),  // 09:21: in at 1,000
		8:  sellAt(0.9), // 09:23: the decider wants out; min_hold refuses
		13: buyAt(0.9),  // 09:28: wants back in; the cooldown refuses
	}}
	r := newRig(t, gapTicks(bankA, 24, 10), d)
	r.cfg.Gate = mustGate(t, stopLimits(nil))
	res := r.run(t)

	cs := cyclesOf(t, r.log, "run1")
	exits := riskExits(cs)
	if len(exits) != 1 || res.Exits != 1 {
		t.Fatalf("risk exits %d (stats %d), want the one stop", len(exits), res.Exits)
	}
	x := exits[0]
	if x.ExitReason != string(risk.ReasonStopLoss) || x.Verdict != string(risk.ReasonStopLoss) || x.Decision != nil ||
		x.Intent == nil || x.Intent.Side != domain.ActionSell || !x.Intent.ReduceOnly || x.Intent.RefPrice != 940 {
		t.Fatalf("exit cycle %+v intent %+v, want a reduce-only SELL at the gapped 940", x, x.Intent)
	}
	if !x.BarClose.Equal(at(9, 25, 30)) {
		t.Errorf("stop at %v, want the gap tick at 09:25:30", x.BarClose)
	}
	for _, want := range []string{"940.00 is through the 980.00 stop", "2.00%", "1000.00 average price", "6.00% adverse"} {
		if !strings.Contains(x.VerdictDetail, want) {
			t.Errorf("detail %q lacks %q", x.VerdictDetail, want)
		}
	}
	if n := countVerdict(cs, string(risk.ReasonMinHold)); n != 1 {
		t.Errorf("min_hold refusals = %d, want the decider's SELL at 09:23 refused", n)
	}
	if n := countVerdict(cs, string(risk.ReasonCooldown)); n != 1 {
		t.Errorf("reentry_cooldown refusals = %d, want the 09:28 BUY refused after the stop", n)
	}
	if len(res.Positions) != 0 || res.OpenOrders != 0 {
		t.Errorf("not flat after the stop: %+v", res)
	}
	var sold bool
	for _, e := range eventsOf(t, r.log, "run1") {
		if e.CycleID == x.CycleID && e.Fill != nil {
			sold = true
			if e.Fill.Side != domain.ActionSell || e.Time.Before(at(9, 25, 30)) {
				t.Errorf("stop fill %+v", e.Fill)
			}
		}
	}
	if !sold {
		t.Error("the stop order never filled")
	}
}

// The same tape without the stop: the position rides down 6% and the decider
// cannot get out, which is the hole issue #59 closes.
func TestWithoutAStopMinHoldRidesTheGapDown(t *testing.T) {
	d := scripted{"I01": {6: buyAt(0.9), 11: sellAt(0.9)}}
	r := newRig(t, gapTicks(bankA, 24, 10), d)
	r.cfg.Gate = mustGate(t, stopLimits(func(l *risk.Limits) { l.StopLossPct = 0 }))
	res := r.run(t)

	if res.Exits != 0 || len(res.Positions) != 1 {
		t.Fatalf("exits %d, positions %d; want the position still held with no stop", res.Exits, len(res.Positions))
	}
	if n := countVerdict(cyclesOf(t, r.log, "run1"), string(risk.ReasonMinHold)); n != 1 {
		t.Errorf("min_hold refusals = %d, want the SELL after the gap refused", n)
	}
}

// A time exit sells at the maximum hold on a flat tape, and the decider was
// never asked to.
func TestTimeExitSellsAtTheMaximumHoldWithoutTheDecider(t *testing.T) {
	d := scripted{"I01": {6: buyAt(0.9)}}
	r := newRig(t, flatTicks(bankA, 1000, 30), d)
	r.cfg.Gate = mustGate(t, stopLimits(func(l *risk.Limits) { l.MaxHold = 10 * time.Minute }))
	res := r.run(t)

	exits := riskExits(cyclesOf(t, r.log, "run1"))
	if len(exits) != 1 || res.Exits != 1 || len(res.Positions) != 0 {
		t.Fatalf("risk exits %d, open positions %d; want one time exit and flat", len(exits), len(res.Positions))
	}
	x := exits[0]
	if x.ExitReason != string(risk.ReasonTimeExit) || !strings.Contains(x.VerdictDetail, "maximum is 10m0s") {
		t.Fatalf("exit %+v, want a time_exit naming the 10m maximum", x)
	}
	// The entry was approved on the bar that closed 09:21 and logged then; the exit is
	// due ten minutes after that, on the first tick at or after.
	if x.BarClose.Before(at(9, 31, 0)) || x.BarClose.After(at(9, 33, 0)) {
		t.Errorf("time exit at %v, want about ten minutes after the 09:21 entry", x.BarClose)
	}
}
