package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// The fixtures below replay the 2026-10-01 churn: RELIANCE at about 1,400
// round-tripped four times inside a 2-rupee range, and each round trip cost
// more than that range could pay.

const relPrice = 1_400.0

func churnGate(t *testing.T, edit func(*Limits)) *Gate {
	t.Helper()
	return budgetGate(t, func(l *Limits) {
		l.CapitalBudget = 100_000
		l.MaxPositionFraction = 1
		l.MinHold, l.ReentryCooldown, l.CostAware = 5*time.Minute, 10*time.Minute, true
		if edit != nil {
			edit(l)
		}
	})
}

// feed marks a flat tape from..to at one price, a minute apart, with a
// total range of rng rupees (one tick at the high, the rest at the low).
func feed(b *Book, token uint32, from time.Time, minutes int, price, rng float64) {
	for i := range minutes {
		p := price
		if i == minutes/2 {
			p = price + rng
		}
		b.MarkAt(token, p, from.Add(time.Duration(i)*time.Minute))
	}
}

func decide(g *Gate, b *Book, token uint32, a domain.Action, p float64, now time.Time) Verdict {
	return g.Evaluate(Request{CycleID: "c-" + now.Format("150405"), Instrument: inst(token), Decision: decision(a, domain.SizeS, p)}, b, now)
}

func TestCostEdgeRefusesAnEntryInsideARangeThatCannotPayTheCosts(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 2) // the 2-rupee range
	v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
	if v.Reason != ReasonCostEdge || v.Approved() {
		t.Fatalf("verdict %+v, want %s", v, ReasonCostEdge)
	}
	// (2×0.8−1) × 2 = 1.20 against 0.20% of 1,400 = 2.80.
	for _, want := range []string{"expected move 1.20", "2.00 range over 30m0s", "2.80", "0.20% of 1400.00"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail %q is missing %q", v.Detail, want)
		}
	}
	if b.Committed() != 0 || b.InFlight(1) {
		t.Fatal("a refused entry left something in the book")
	}
}

func TestCostEdgeAllowsAnEntryWhenTheMoveBeatsCosts(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 12) // (2×0.8−1) × 12 = 7.20 ≥ 2.80
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("an entry with a 7.20 expected move was refused: %+v", v)
	}
}

func TestCostEdgeScalesWithProbability(t *testing.T) {
	g := churnGate(t, nil)
	// A 6-rupee range pays 2.80 only if 2p−1 ≥ 0.4667, that is p ≥ 0.7334.
	for _, c := range []struct {
		p    float64
		want bool
	}{{0.65, false}, {0.70, false}, {0.74, true}, {0.95, true}} {
		b := NewBook()
		feed(b, 1, at(9, 30), 30, relPrice, 6)
		if got := decide(g, b, 1, domain.ActionBuy, c.p, at(10, 0)).Approved(); got != c.want {
			t.Errorf("probability %.2f approved = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestCostEdgeMarginIsConfigurable(t *testing.T) {
	b := func() *Book { b := NewBook(); feed(b, 1, at(9, 30), 30, relPrice, 5); return b } // 0.8 → 3.00
	if v := decide(churnGate(t, nil), b(), 1, domain.ActionBuy, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("3.00 against costs and margin of 2.80 was refused: %+v", v)
	}
	if v := decide(churnGate(t, func(l *Limits) { l.CostMargin = 0.001 }), b(), 1, domain.ActionBuy, 0.8, at(10, 0)); v.Reason != ReasonCostEdge {
		t.Fatalf("a 0.10%% margin needs 4.20; verdict %+v", v)
	}
}

func TestCostEdgeUsesOnlyTheRangeWindow(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	feed(b, 1, at(9, 20), 5, relPrice+40, 0) // a big move, but 40+ minutes ago
	feed(b, 1, at(9, 30), 30, relPrice, 2)
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)); v.Reason != ReasonCostEdge {
		t.Fatalf("a move outside the window counted: %+v", v)
	}
	g = churnGate(t, func(l *Limits) { l.RangeWindow = 2 * time.Hour })
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("a wider window should see the earlier 40-rupee move: %+v", v)
	}
}

func TestCostEdgeCountsBarHighsAndLows(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	for i := range 30 {
		end := at(9, 31+i)
		b.MarkBar(domain.Bar{Instrument: inst(1), Interval: time.Minute, Start: end.Add(-time.Minute), End: end,
			Open: relPrice, High: relPrice + 8, Low: relPrice - 8, Close: relPrice})
	}
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 1)); !v.Approved() { // 0.6 × 16 = 9.6
		t.Fatalf("bar ranges were ignored: %+v", v)
	}
}

func TestCostEdgeNeedsPriceHistoryAndCanBeSwitchedOff(t *testing.T) {
	b := NewBook()
	b.Mark(1, relPrice) // untimed: no range to judge by
	if v := decide(churnGate(t, nil), b, 1, domain.ActionBuy, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("no price history should not block an entry: %+v", v)
	}
	b = NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 2)
	if v := decide(churnGate(t, func(l *Limits) { l.CostAware = false }), b, 1, domain.ActionBuy, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("the check is off: %+v", v)
	}
}

func TestCostEdgeDoesNotHoldBackExits(t *testing.T) {
	g := churnGate(t, func(l *Limits) { l.MinHold = 0 })
	b := NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 2)
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: 10, AvgPrice: relPrice})
	if v := decide(g, b, 1, domain.ActionSell, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("an exit was held to the cost check: %+v", v)
	}
}

func TestMinHoldRefusesAnEarlyDeciderExit(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 12)
	a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)).Approval()
	if !ok {
		t.Fatal("entry refused")
	}
	fill(b, a)

	v := decide(g, b, 1, domain.ActionSell, 0.9, at(10, 3))
	if v.Reason != ReasonMinHold || v.Approved() {
		t.Fatalf("verdict %+v, want %s", v, ReasonMinHold)
	}
	for _, want := range []string{"held 3m0s of the minimum 5m0s", "10:00:00"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail %q is missing %q", v.Detail, want)
		}
	}
	if b.InFlight(1) {
		t.Fatal("a refused exit is in flight")
	}
	if v := decide(g, b, 1, domain.ActionSell, 0.9, at(10, 5)); !v.Approved() {
		t.Fatalf("an exit after the minimum hold was refused: %+v", v)
	}
}

func TestMinHoldNeverDelaysRiskExits(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	feed(b, 1, at(14, 20), 30, relPrice, 12)
	a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(14, 50)).Approval()
	if !ok {
		t.Fatal("entry refused")
	}
	fill(b, a)

	// The kill switch and the square-off flatten a position held for seconds.
	b.Kill(ReasonKillFile)
	if plan := g.Exits(b, at(14, 50).Add(10*time.Second)); plan.Reason != ReasonKillSwitch || len(plan.Approvals) != 1 {
		t.Fatalf("kill switch exit plan = %+v", plan)
	}

	b = NewBook()
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: 5, AvgPrice: relPrice})
	b.Mark(1, relPrice)
	b.noteEntry(1, 0.8, at(15, 9))
	if plan := g.Exits(b, at(15, 10)); plan.Reason != ReasonSquareOff || len(plan.Approvals) != 1 {
		t.Fatalf("square-off exit plan = %+v", plan)
	}
}

func TestMinHoldIgnoresPositionsWithoutAnEntryTime(t *testing.T) {
	g := churnGate(t, nil)
	b := NewBook()
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: 5, AvgPrice: relPrice}) // restored from the journal
	b.Mark(1, relPrice)
	if v := decide(g, b, 1, domain.ActionSell, 0.8, at(10, 0)); !v.Approved() {
		t.Fatalf("a restored position was held back: %+v", v)
	}
}

func TestMinHoldIsMeasuredFromTheFirstEntry(t *testing.T) {
	g := churnGate(t, func(l *Limits) { l.CostAware = false })
	b := NewBook()
	b.Mark(1, relPrice)
	for _, m := range []int{0, 4} {
		a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, m)).Approval()
		if !ok {
			t.Fatalf("entry at +%dm refused", m)
		}
		fill(b, a)
	}
	if v := decide(g, b, 1, domain.ActionSell, 0.8, at(10, 5)); !v.Approved() {
		t.Fatalf("five minutes after the first entry the exit is due: %+v", v)
	}
}

func TestReentryCooldownAfterAnExit(t *testing.T) {
	g := churnGate(t, func(l *Limits) { l.CostAware = false })
	b := NewBook()
	b.Mark(1, relPrice)
	a, _ := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0)).Approval()
	fill(b, a)
	a, ok := decide(g, b, 1, domain.ActionSell, 0.8, at(10, 6)).Approval()
	if !ok {
		t.Fatal("exit refused")
	}
	fill(b, a)

	v := decide(g, b, 1, domain.ActionBuy, 0.9, at(10, 9))
	if v.Reason != ReasonCooldown || v.Approved() {
		t.Fatalf("verdict %+v, want %s", v, ReasonCooldown)
	}
	for _, want := range []string{"sold out of token 1 at 10:06:00", "3m0s ago", "after 10m0s"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail %q is missing %q", v.Detail, want)
		}
	}
	// Another instrument is not affected, and this one is free again after the cooldown.
	b.Mark(2, 500)
	if v := decide(g, b, 2, domain.ActionBuy, 0.9, at(10, 9)); !v.Approved() {
		t.Fatalf("the cooldown leaked to another instrument: %+v", v)
	}
	if v := decide(g, b, 1, domain.ActionBuy, 0.9, at(10, 16)); !v.Approved() {
		t.Fatalf("re-entry after the cooldown was refused: %+v", v)
	}
}

func TestReentryCooldownFollowsRiskExitsAndRotation(t *testing.T) {
	g := churnGate(t, func(l *Limits) { l.CostAware = false; l.MinHold = 0; l.RotationMinHold = 0 })
	b := NewBook()
	b.Mark(1, relPrice)
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: 5, AvgPrice: relPrice})
	b.Kill(ReasonKillFile)
	g.Exits(b, at(10, 0))
	if b.exits[1] != at(10, 0) {
		t.Fatalf("a risk exit did not start the cooldown: %v", b.exits)
	}

}

func TestReentryCooldownFollowsARotation(t *testing.T) {
	g, b := fullBook(t, 0.70, 0.62, 0.75, 0.66, 0.90) // token 11 is the weakest
	g.limits.ReentryCooldown = 10 * time.Minute
	if v := buy(g, b, 1, 1_000, domain.SizeS, 0.88, at(11, 0)); v.Reason != ReasonRotation {
		t.Fatalf("verdict %+v, want a rotation", v)
	}
	if got := b.exits[11]; !got.Equal(at(11, 0)) {
		t.Fatalf("the rotated-out instrument's exit time = %v, want 11:00", got)
	}
}

func TestChurnChecksAreOffWhenZero(t *testing.T) {
	g := churnGate(t, func(l *Limits) { l.MinHold, l.ReentryCooldown, l.CostAware = 0, 0, false })
	b := NewBook()
	feed(b, 1, at(9, 30), 30, relPrice, 2)
	for i := range 4 { // the RELIANCE churn goes through untouched
		a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 2*i)).Approval()
		if !ok {
			t.Fatalf("round trip %d: entry refused", i)
		}
		fill(b, a)
		a, ok = decide(g, b, 1, domain.ActionSell, 0.8, at(10, 2*i+1)).Approval()
		if !ok {
			t.Fatalf("round trip %d: exit refused", i)
		}
		fill(b, a)
	}
}

// TestDefaultsStopTheRelianceChurn runs the default limits over the churning
// tape: one entry at most, and no repeat round trips inside the range.
func TestDefaultsStopTheRelianceChurn(t *testing.T) {
	g, err := NewGate(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	b := NewBook()
	feed(b, 1, at(9, 20), 40, relPrice, 2)
	var entries int
	for m := 0; m < 120; m++ {
		now := at(10, 0).Add(time.Duration(m) * time.Minute)
		b.MarkAt(1, relPrice+float64(m%2), now)
		for _, a := range []domain.Action{domain.ActionBuy, domain.ActionSell} {
			if ap, ok := decide(g, b, 1, a, 0.7, now).Approval(); ok {
				if !ap.Intent().ReduceOnly {
					entries++
				}
				fill(b, ap)
			}
		}
	}
	if entries != 0 {
		t.Fatalf("%d entries inside a 2-rupee range at probability 0.7", entries)
	}
}

func TestBookKeepsAMinuteBucketedRange(t *testing.T) {
	b := NewBook()
	b.MarkAt(1, 100, at(10, 0))
	b.MarkAt(1, 103, at(10, 0).Add(20*time.Second))
	b.MarkAt(1, 98, at(10, 1))
	b.MarkAt(1, 99, at(10, 0).Add(40*time.Second)) // out of order, still the 10:00 bucket
	if r, ok := b.recentRange(1, at(10, 2), 30*time.Minute); !ok || r != 5 {
		t.Fatalf("range = %v, %v; want 5", r, ok)
	}
	if _, ok := b.recentRange(2, at(10, 2), time.Hour); ok {
		t.Fatal("range for an instrument never marked")
	}
	b.MarkAt(1, 90, at(13, 0)) // far past the keep window: old buckets go
	if n := len(b.ranges[1]); n != 1 {
		t.Fatalf("%d buckets kept, want 1", n)
	}
	b.Mark(3, 100) // an untimed mark feeds no range
	if _, ok := b.recentRange(3, at(10, 2), time.Hour); ok {
		t.Fatal("an untimed mark fed the range")
	}
}
