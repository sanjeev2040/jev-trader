package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// The fixtures below are the left tail the critique (reports/0006) found: a
// catalyst name that drops 4-5% in the first minutes while the minimum hold
// keeps the decider from selling it for an hour.

const stopPrice = 100.0

// stopGate is a gate with the trade-for-hours rules on (an hour's minimum hold
// and cooldown, two confirmations) and a 2% stop, on the 25,000 budget.
func stopGate(t *testing.T, edit func(*Limits)) *Gate {
	t.Helper()
	return holdGate(t, func(l *Limits) {
		l.StopLossPct = 0.02
		if edit != nil {
			edit(l)
		}
	})
}

// openLong books a long in token at price through the gate at now, filled at
// the entry price, and returns the book. The entry is two confirmed calls: the
// first at now minus five minutes is only the first of the two.
func openLong(t *testing.T, g *Gate, b *Book, token uint32, price float64, now time.Time) {
	t.Helper()
	b.Mark(token, price)
	decide(g, b, token, domain.ActionBuy, 0.8, now.Add(-5*time.Minute))
	a, ok := decide(g, b, token, domain.ActionBuy, 0.8, now).Approval()
	if !ok {
		t.Fatalf("entry in token %d at %.2f refused", token, price)
	}
	fill(b, a)
}

func TestDefaultsHaveAStopAndNoTimeExit(t *testing.T) {
	l := DefaultLimits()
	if l.StopLossPct != 0.02 || l.StopRangeMult != 0 || l.MaxHold != 0 {
		t.Fatalf("stop %v, range multiple %v, max hold %s; want a 2%% stop and no time exit by default", l.StopLossPct, l.StopRangeMult, l.MaxHold)
	}
}

// The stop is placed by Gate.Exits, not asked of a decider, so the hour's
// minimum hold that refuses the decider's own SELL does not touch it.
func TestStopExitsDuringTheMinimumHold(t *testing.T) {
	g := stopGate(t, nil)
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	pos, _ := b.Position(1)
	if pos.Quantity != 12 {
		t.Fatalf("setup position %+v", pos)
	}

	b.Mark(1, 97.5) // -2.5% three minutes in
	now := at(10, 3)
	if v := decide(g, b, 1, domain.ActionSell, 0.95, now); v.Reason != ReasonMinHold {
		t.Fatalf("the decider's SELL got %+v, want min_hold: the premise of the stop is that it cannot", v)
	}
	plan := g.Exits(b, now)
	if plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want one stop_loss exit", plan)
	}
	a := plan.Approvals[0]
	in := a.Intent()
	if a.Source() != SourceRisk || a.Reason() != ReasonStopLoss || in.Side != domain.ActionSell || in.Quantity != 12 || !in.ReduceOnly || in.RefPrice != 97.5 {
		t.Fatalf("exit %s %s %+v, want a reduce-only SELL of 12 at 97.50", a.Source(), a.Reason(), in)
	}
	if !strings.HasPrefix(in.CycleID, "risk-stop_loss-1-") {
		t.Errorf("cycle id %q", in.CycleID)
	}
	for _, want := range []string{"97.50", "98.00 stop", "2.00%", "100.00 average", "2.50% adverse"} {
		if !strings.Contains(a.Detail(), want) {
			t.Errorf("detail %q lacks %q", a.Detail(), want)
		}
	}
	if !b.InFlight(1) {
		t.Error("the exit is not in flight in the book")
	}
	if again := g.Exits(b, now.Add(time.Second)); len(again.Approvals) != 0 {
		t.Errorf("the stop repeated while its order is in flight: %+v", again)
	}
}

func TestStopWaitsForTheStopLevel(t *testing.T) {
	g := stopGate(t, nil)
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	b.Mark(1, 98.01) // 1.99% down
	if plan := g.Exits(b, at(10, 5)); plan.Reason != "" || len(plan.Approvals) != 0 {
		t.Fatalf("exit before the stop level: %+v", plan)
	}
	b.Mark(1, 98) // exactly on the level
	if plan := g.Exits(b, at(10, 6)); plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 {
		t.Fatalf("no exit on the stop level: %+v", plan)
	}
}

// A price that jumps over the stop level exits at the price it jumped to, not
// at the level: the order's reference is the last mark.
func TestStopOnAGapExitsAtTheGappedPrice(t *testing.T) {
	g := stopGate(t, nil)
	b := NewBook()
	b.MarkAt(1, stopPrice, at(10, 0))
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	b.MarkAt(1, 99.5, at(10, 1))
	if plan := g.Exits(b, at(10, 1)); len(plan.Approvals) != 0 {
		t.Fatalf("exit at -0.5%%: %+v", plan)
	}
	b.MarkAt(1, 90, at(10, 2)) // the next tick is 10% lower
	plan := g.Exits(b, at(10, 2))
	if plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want a stop exit after the gap", plan)
	}
	a := plan.Approvals[0]
	if a.Intent().RefPrice != 90 || !strings.Contains(a.Detail(), "90.00 is through the 98.00 stop") || !strings.Contains(a.Detail(), "10.00% adverse") {
		t.Errorf("exit %+v detail %q, want the gapped 90.00 against the 98.00 stop", a.Intent(), a.Detail())
	}
}

func TestStopOnAShort(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.AllowShort = true })
	b := NewBook()
	b.Mark(1, stopPrice)
	decide(g, b, 1, domain.ActionSell, 0.8, at(10, 0))
	a, ok := decide(g, b, 1, domain.ActionSell, 0.8, at(10, 5)).Approval()
	if !ok {
		t.Fatal("short entry refused")
	}
	fill(b, a)
	if pos, _ := b.Position(1); pos.Quantity != -12 {
		t.Fatalf("position %+v", pos)
	}
	b.Mark(1, 101.9)
	if plan := g.Exits(b, at(10, 6)); len(plan.Approvals) != 0 {
		t.Fatalf("a short stopped out at +1.9%%: %+v", plan)
	}
	b.Mark(1, 102.5)
	plan := g.Exits(b, at(10, 7))
	if plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 || plan.Approvals[0].Intent().Side != domain.ActionBuy || plan.Approvals[0].Intent().Quantity != 12 {
		t.Fatalf("plan %+v, want a BUY of 12 to cover", plan)
	}
}

// The range stop widens the distance for a volatile stock, and is fixed when
// the position is opened: the range a fall creates does not widen it.
func TestRangeStopIsTheLargerOfThePercentAndTheRangeAndIsFixedAtEntry(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.StopRangeMult = 1.5; l.RangeWindow = 30 * time.Minute })
	b := NewBook()
	feed(b, 1, at(9, 30), 30, stopPrice, 4) // a 4-rupee range at 100: 1.5 x 4 = 6%, more than 2%
	openLong(t, g, b, 1, stopPrice, at(10, 0))

	b.MarkAt(1, 95, at(10, 5)) // -5%: through a 2% stop, inside a 6% one
	if plan := g.Exits(b, at(10, 5)); len(plan.Approvals) != 0 {
		t.Fatalf("stopped at -5%% with a 6%% range stop: %+v", plan)
	}
	b.MarkAt(1, 93.9, at(10, 6)) // the fall has now put a 10-rupee range in the window; the stop must not follow it
	plan := g.Exits(b, at(10, 6))
	if plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 || !strings.Contains(plan.Approvals[0].Detail(), "94.00 stop, 6.00%") {
		t.Fatalf("plan %+v, want the stop at 94.00 (6%%)", plan)
	}

	// A quiet stock keeps the percentage.
	q := NewBook()
	feed(q, 2, at(9, 30), 30, stopPrice, 0.4) // 1.5 x 0.4 = 0.6%, less than 2%
	openLong(t, g, q, 2, stopPrice, at(10, 0))
	q.MarkAt(2, 97.9, at(10, 5))
	if plan := g.Exits(q, at(10, 5)); plan.Reason != ReasonStopLoss || !strings.Contains(plan.Approvals[0].Detail(), "98.00 stop, 2.00%") {
		t.Fatalf("plan %+v, want the 2%% stop at 98.00", plan)
	}
}

func TestStopIsOffWhenBothAreZero(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.StopLossPct, l.StopRangeMult = 0, 0 })
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	b.Mark(1, 50)
	if plan := g.Exits(b, at(10, 5)); plan.Reason != "" || len(plan.Approvals) != 0 {
		t.Fatalf("a position was stopped with the stop off: %+v", plan)
	}
}

// A position the book has no entry for, restored from the journal, gets the
// fixed stop; it has no entry time, so no time exit.
func TestRestoredPositionGetsTheFixedStopButNoTimeExit(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = time.Hour })
	b := NewBook()
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: 5, AvgPrice: stopPrice})
	b.Mark(1, 99)
	if plan := g.Exits(b, at(14, 0)); len(plan.Approvals) != 0 {
		t.Fatalf("a restored position was timed out: %+v", plan)
	}
	b.Mark(1, 97)
	if plan := g.Exits(b, at(14, 1)); plan.Reason != ReasonStopLoss || len(plan.Approvals) != 1 {
		t.Fatalf("a restored position was not stopped: %+v", plan)
	}
}

func TestTimeExitAtTheMaximumHoldIgnoresTheMinimumHold(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = 2 * time.Hour })
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	b.Mark(1, 101) // in profit: only the clock is acting

	if plan := g.Exits(b, at(11, 59)); len(plan.Approvals) != 0 {
		t.Fatalf("time exit a minute early: %+v", plan)
	}
	plan := g.Exits(b, at(12, 0))
	if plan.Reason != ReasonTimeExit || len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want one time_exit", plan)
	}
	a := plan.Approvals[0]
	if a.Source() != SourceRisk || a.Reason() != ReasonTimeExit || a.Intent().Side != domain.ActionSell || a.Intent().Quantity != 12 || !a.Intent().ReduceOnly {
		t.Fatalf("exit %s %s %+v", a.Source(), a.Reason(), a.Intent())
	}
	for _, want := range []string{"held 2h0m0s", "10:00:00", "maximum is 2h0m0s"} {
		if !strings.Contains(a.Detail(), want) {
			t.Errorf("detail %q lacks %q", a.Detail(), want)
		}
	}
}

// A time exit shorter than the minimum hold is allowed and wins: it is a risk
// exit, not a decider's.
func TestTimeExitCanBeShorterThanTheMinimumHold(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = 20 * time.Minute })
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	if v := decide(g, b, 1, domain.ActionSell, 0.9, at(10, 21)); v.Reason != ReasonMinHold {
		t.Fatalf("decider exit %+v, want min_hold", v)
	}
	if plan := g.Exits(b, at(10, 21)); plan.Reason != ReasonTimeExit || len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want the time exit", plan)
	}
}

func TestTimeExitIsMeasuredFromTheFirstEntry(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = time.Hour; l.EntryConfirmations = 0 })
	b := NewBook()
	b.Mark(1, stopPrice)
	for _, m := range []int{0, 30} {
		a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, m)).Approval()
		if !ok {
			t.Fatalf("entry at +%dm refused", m)
		}
		fill(b, a)
	}
	if plan := g.Exits(b, at(10, 59)); len(plan.Approvals) != 0 {
		t.Fatalf("exit early: %+v", plan)
	}
	if plan := g.Exits(b, at(11, 0)); plan.Reason != ReasonTimeExit {
		t.Fatalf("plan %+v, want a time exit an hour after the first entry, not the add", plan)
	}
}

func TestStopBeatsTimeExitWhenBothAreDue(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = time.Hour })
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	b.Mark(1, 90)
	if plan := g.Exits(b, at(11, 0)); plan.Reason != ReasonStopLoss {
		t.Fatalf("plan %+v, want the stop named", plan)
	}
}

// Book-wide exits keep their reason and still take everything.
func TestSquareOffAndKillSwitchStayBookWide(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = 8 * time.Hour })
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	openLong(t, g, b, 2, 200, at(10, 0))
	b.Mark(1, 90) // stopped
	b.Mark(2, 205)
	plan := g.Exits(b, at(15, 10))
	if plan.Reason != ReasonSquareOff || len(plan.Approvals) != 2 {
		t.Fatalf("plan %+v, want both flattened by the square-off", plan)
	}
	for _, a := range plan.Approvals {
		if a.Reason() != ReasonSquareOff || a.Detail() != "" {
			t.Errorf("approval reason %s detail %q, want square_off", a.Reason(), a.Detail())
		}
	}

	k := NewBook()
	openLong(t, g, k, 1, stopPrice, at(10, 0))
	k.Mark(1, 90)
	k.Kill(ReasonKillFile)
	if plan := g.Exits(k, at(10, 5)); plan.Reason != ReasonKillSwitch || plan.Approvals[0].Reason() != ReasonKillSwitch {
		t.Fatalf("plan %+v, want the kill switch named", plan)
	}
}

// Only the position through its stop is sold.
func TestStopTakesOnlyTheStoppedPosition(t *testing.T) {
	g := stopGate(t, nil)
	b := NewBook()
	openLong(t, g, b, 1, stopPrice, at(10, 0))
	openLong(t, g, b, 2, 200, at(10, 0))
	b.Mark(1, 96)
	b.Mark(2, 201)
	plan := g.Exits(b, at(10, 10))
	if len(plan.Approvals) != 1 || plan.Approvals[0].Intent().Instrument.Token != 1 {
		t.Fatalf("plan %+v, want only token 1", plan)
	}
	if b.InFlight(2) {
		t.Error("the healthy position has an order in flight")
	}
}

// What the bot never opened is never stopped.
func TestStopNeverTouchesAForeignHolding(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.MaxHold = time.Minute })
	b := NewBook()
	b.SetForeign(domain.Position{Instrument: inst(7), Quantity: 100, AvgPrice: 500})
	b.Mark(7, 100) // down 80%
	if plan := g.Exits(b, at(14, 0)); plan.Reason != "" || len(plan.Approvals) != 0 {
		t.Fatalf("a foreign holding was exited: %+v", plan)
	}
}

// A stop is a risk exit: it does not use the daily order budget, but it
// does keep to the order-rate limit and goes on a later call.
func TestStopRespectsOrderRateButNotTheDailyBudget(t *testing.T) {
	g := stopGate(t, func(l *Limits) {
		l.MaxOrdersPerSecond, l.MaxOrdersPerMinute, l.MaxOrdersPerDay = 1, 4, 4
		l.EntryConfirmations = 0
	})
	b := NewBook()
	for tok := uint32(1); tok <= 2; tok++ {
		b.Mark(tok, stopPrice)
		a, ok := decide(g, b, tok, domain.ActionBuy, 0.8, at(10, int(tok)-1)).Approval()
		if !ok {
			t.Fatalf("setup entry %d refused", tok)
		}
		fill(b, a)
	}
	b.dayOrders = 4 // the decision budget is spent
	b.Mark(1, 90)
	b.Mark(2, 90)
	now := at(10, 20)
	if got := len(g.Exits(b, now).Approvals); got != 1 {
		t.Fatalf("%d exits in the first second, want 1 under a limit of 1 a second", got)
	}
	if got := len(g.Exits(b, now.Add(2*time.Second)).Approvals); got != 1 {
		t.Fatalf("%d exits two seconds later, want the other one", got)
	}
	if b.dayOrders != 4 {
		t.Errorf("exits used the decision-order budget: %d", b.dayOrders)
	}
}

// A stop that finds an add-on entry still working cancels it, and sells once
// it is gone.
func TestStopCancelsAnInFlightAddOnFirst(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.EntryConfirmations = 0 })
	b := NewBook()
	b.Mark(1, stopPrice)
	a, _ := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)).Approval()
	fill(b, a)
	if !decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 5)).Approved() { // an add, still working
		t.Fatal("add-on refused")
	}
	b.Mark(1, 95)
	plan := g.Exits(b, at(10, 6))
	if plan.Reason != ReasonStopLoss || len(plan.Approvals) != 0 || len(plan.Cancel) != 1 || plan.Cancel[0].ReduceOnly {
		t.Fatalf("plan %+v, want the add-on cancelled and no exit yet", plan)
	}
	b.Settle(1) // the cancel lands
	if plan := g.Exits(b, at(10, 6).Add(time.Second)); len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want the exit once the add-on is gone", plan)
	}
}

// After a stop the stock is out of bounds for the cooldown, however strong the
// next calls.
func TestStopStartsTheReentryCooldown(t *testing.T) {
	g := stopGate(t, func(l *Limits) { l.EntryConfirmations = 0 })
	b := NewBook()
	b.Mark(1, stopPrice)
	a, _ := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)).Approval()
	fill(b, a)
	b.Mark(1, 95)
	plan := g.Exits(b, at(10, 5))
	if len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v", plan)
	}
	fill(b, plan.Approvals[0])
	if v := decide(g, b, 1, domain.ActionBuy, 0.99, at(10, 30)); v.Reason != ReasonCooldown {
		t.Fatalf("re-entry after a stop got %+v, want the cooldown", v)
	}
	if v := decide(g, b, 1, domain.ActionBuy, 0.99, at(11, 5)); !v.Approved() {
		t.Fatalf("re-entry after the cooldown refused: %+v", v)
	}
}

// fullBudget is two 1,000 positions, the weaker (probability 0.71) in token 1,
// in a budget of 3,000 that has room for no third, opened at 10:00 and 10:01.
func fullBudget(t *testing.T) (*Gate, *Book) {
	t.Helper()
	g := stopGate(t, func(l *Limits) {
		l.CapitalBudget = 3_000
		l.MaxPositionFraction = 1
		l.EntryConfirmations = 0
		l.RotationMinHold = 30 * time.Minute
	})
	b := NewBook()
	for tok := uint32(1); tok <= 2; tok++ {
		b.Mark(tok, 1_000)
		a, ok := decide(g, b, tok, domain.ActionBuy, 0.7+0.01*float64(tok), at(10, int(tok)-1)).Approval()
		if !ok {
			t.Fatalf("setup entry %d refused", tok)
		}
		fill(b, a)
	}
	b.Mark(3, 1_000)
	return g, b
}

// With the budget full, a stop frees the capital and the next entry fits at
// once, with no rotation and no waiting for the minimum hold.
func TestStopFreesTheBudgetForTheNextEntry(t *testing.T) {
	g, b := fullBudget(t)
	if v := decide(g, b, 3, domain.ActionBuy, 0.99, at(10, 10)); v.Approved() || v.Reason != ReasonBudgetFull {
		t.Fatalf("premise: with the budget full and nothing old enough to rotate the entry got %+v", v)
	}
	b.Mark(1, 950)
	plan := g.Exits(b, at(10, 11))
	if len(plan.Approvals) != 1 || plan.Approvals[0].Intent().Instrument.Token != 1 {
		t.Fatalf("plan %+v, want a stop in token 1", plan)
	}
	fill(b, plan.Approvals[0])
	v := decide(g, b, 3, domain.ActionBuy, 0.99, at(10, 13))
	if !v.Approved() || v.Reason != ReasonApproved {
		t.Fatalf("after the stop filled the entry got %+v, want it approved into the freed capital without a rotation", v)
	}
}

// Rotation never picks a position whose stop exit is already working: the
// weakest position is being sold, so the entry rotates the next one out.
func TestRotationSkipsAPositionBeingStopped(t *testing.T) {
	g, b := fullBudget(t)
	b.Mark(1, 950)
	if plan := g.Exits(b, at(10, 31)); len(plan.Approvals) != 1 {
		t.Fatalf("plan %+v, want a stop in token 1", plan)
	}
	v := decide(g, b, 3, domain.ActionBuy, 0.99, at(10, 32))
	a, ok := v.Rotation()
	if v.Reason != ReasonRotation || !ok || a.Intent().Instrument.Token != 2 {
		t.Fatalf("verdict %+v, want a rotation of token 2: token 1 is already being sold", v)
	}
}
