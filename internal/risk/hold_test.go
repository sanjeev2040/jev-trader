package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// holdGate is a gate with the trade-for-hours rules on: an entry needs 0.70
// and two calls in a row, a position is held an hour, a sale is followed by an
// hour's cooldown. The cost check is off so the tests are about the calls.
func holdGate(t *testing.T, edit func(*Limits)) *Gate {
	t.Helper()
	return budgetGate(t, func(l *Limits) {
		def := DefaultLimits()
		l.MinHold, l.ReentryCooldown, l.RotationMinHold = def.MinHold, def.ReentryCooldown, def.RotationMinHold
		l.EntryMinProbability, l.EntryConfirmations = def.EntryMinProbability, def.EntryConfirmations
		if edit != nil {
			edit(l)
		}
	})
}

func TestDefaultsTradeForHours(t *testing.T) {
	l := DefaultLimits()
	if l.MinHold != time.Hour || l.ReentryCooldown != time.Hour || l.RotationMinHold != time.Hour {
		t.Errorf("hold %s, cooldown %s, rotation hold %s; want an hour each", l.MinHold, l.ReentryCooldown, l.RotationMinHold)
	}
	if l.EntryMinProbability != 0.70 || l.EntryConfirmations != 2 {
		t.Errorf("entry threshold %v on %d calls; want 0.70 on 2", l.EntryMinProbability, l.EntryConfirmations)
	}
	if l.MinProbability[domain.ActionSell] != 0.60 {
		t.Errorf("the exit threshold moved to %v; only entries get stricter", l.MinProbability[domain.ActionSell])
	}
}

func TestEntryNeedsTheHigherThresholdAndRepeatedCalls(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 1_000)

	// 0.65 clears the base 0.60 but not the entry's 0.70, and does not count.
	if v := decide(g, b, 1, domain.ActionBuy, 0.65, at(10, 0)); v.Reason != ReasonBelowThreshold || !strings.Contains(v.Detail, "below 0.7000") {
		t.Fatalf("verdict %+v, want below_probability_threshold at 0.70", v)
	}
	// The first strong call is only the first of two.
	v := decide(g, b, 1, domain.ActionBuy, 0.75, at(10, 5))
	if v.Reason != ReasonUnconfirmed || v.Approved() || !strings.Contains(v.Detail, "call 1 of 2") {
		t.Fatalf("verdict %+v, want entry_unconfirmed, call 1 of 2", v)
	}
	if b.Committed() != 0 || b.InFlight(1) {
		t.Fatal("an unconfirmed entry left something in the book")
	}
	if v := decide(g, b, 1, domain.ActionBuy, 0.72, at(10, 10)); !v.Approved() {
		t.Fatalf("the second call in a row was refused: %+v", v)
	}
}

func TestAnythingButTheSameCallStartsTheCountAgain(t *testing.T) {
	for name, between := range map[string]func(*Gate, *Book) Verdict{
		"a HOLD": func(g *Gate, b *Book) Verdict {
			return g.Evaluate(Request{CycleID: "h", Instrument: inst(1), Decision: decision(domain.ActionHold, "", 0.9)}, b, at(10, 5))
		},
		"a weaker call": func(g *Gate, b *Book) Verdict { return decide(g, b, 1, domain.ActionBuy, 0.65, at(10, 5)) },
		"the other action": func(g *Gate, b *Book) Verdict {
			return g.Evaluate(Request{CycleID: "s", Instrument: inst(1), Decision: decision(domain.ActionSell, domain.SizeS, 0.9)}, b, at(10, 5))
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := holdGate(t, nil)
			b := NewBook()
			b.Mark(1, 1_000)
			decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
			between(g, b)
			if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 10)); v.Reason != ReasonUnconfirmed {
				t.Fatalf("after %s the next BUY got %+v, want it to count as call 1 again", name, v)
			}
			if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 15)); !v.Approved() {
				t.Fatalf("two BUYs in a row after %s were refused: %+v", name, v)
			}
		})
	}
}

func TestConfirmationIsPerInstrumentAndPerDay(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 1_000)
	b.Mark(2, 1_000)
	decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
	if v := decide(g, b, 2, domain.ActionBuy, 0.8, at(10, 5)); v.Reason != ReasonUnconfirmed {
		t.Fatalf("a call on another instrument confirmed this one: %+v", v)
	}
	// A call left unconfirmed at the close is not confirmation the next morning.
	next := at(10, 0).AddDate(0, 0, 1)
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, next); v.Reason != ReasonUnconfirmed {
		t.Fatalf("yesterday's call confirmed today's: %+v", v)
	}
}

func TestAddingToAPositionNeedsFreshConfirmation(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 100)
	decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
	a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 5)).Approval()
	if !ok {
		t.Fatal("setup entry refused")
	}
	fill(b, a)
	if v := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 10)); v.Reason != ReasonUnconfirmed {
		t.Fatalf("an add right after the entry got %+v, want a new count", v)
	}
}

func TestExitsAreNotHeldToTheEntryBar(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 100)
	decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
	a, ok := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 5)).Approval()
	if !ok {
		t.Fatal("setup entry refused")
	}
	fill(b, a)
	// 0.62 clears the 0.60 exit threshold, needs no confirmation, and is
	// allowed once the hour is up.
	if v := decide(g, b, 1, domain.ActionSell, 0.62, at(10, 5).Add(time.Hour)); !v.Approved() {
		t.Fatalf("a 0.62 exit after the hold was refused: %+v", v)
	}
}

func TestHourLongHoldStillYieldsToRiskExits(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 100)
	decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 0))
	a, _ := decide(g, b, 1, domain.ActionBuy, 0.8, at(10, 5)).Approval()
	fill(b, a)
	if v := decide(g, b, 1, domain.ActionSell, 0.9, at(10, 30)); v.Reason != ReasonMinHold {
		t.Fatalf("a decider exit after 25 minutes got %+v, want min_hold", v)
	}
	b.Kill(ReasonKillFile)
	if plan := g.Exits(b, at(10, 31)); len(plan.Approvals) != 1 {
		t.Fatalf("the kill switch did not flatten at once: %+v", plan)
	}
}

func TestSquareOffIgnoresTheHold(t *testing.T) {
	g := holdGate(t, nil)
	b := NewBook()
	b.Mark(1, 100)
	decide(g, b, 1, domain.ActionBuy, 0.8, at(14, 30))
	a, _ := decide(g, b, 1, domain.ActionBuy, 0.8, at(14, 35)).Approval()
	fill(b, a)
	if plan := g.Exits(b, at(15, 10)); plan.Reason != ReasonSquareOff || len(plan.Approvals) != 1 {
		t.Fatalf("square-off 35 minutes into the hold: %+v", plan)
	}
}

func TestRotationFollowsTheMinimumHold(t *testing.T) {
	g := holdGate(t, func(l *Limits) { l.MaxPositionFraction = 0.25 })
	b := NewBook()
	for i, p := range []float64{0.70, 0.62, 0.75, 0.66} {
		tok := uint32(10 + i)
		b.Mark(tok, 100)
		decide(g, b, tok, domain.ActionBuy, p+0.1, at(10, i*2))
		a, ok := g.Evaluate(Request{CycleID: "e", Instrument: inst(tok), Decision: decision(domain.ActionBuy, domain.SizeL, p+0.1)}, b, at(10, i*2+1)).Approval()
		if !ok {
			t.Fatalf("setup entry %d refused", i)
		}
		fill(b, a)
	}
	b.Mark(1, 5_000) // one share does not fit in what is left of the budget
	decide(g, b, 1, domain.ActionBuy, 0.99, at(10, 20))
	if v := decide(g, b, 1, domain.ActionBuy, 0.99, at(10, 25)); v.Reason == ReasonRotation {
		t.Fatalf("rotated out a position held 25 minutes: %+v", v)
	}
	decide(g, b, 1, domain.ActionBuy, 0.99, at(11, 20))
	if v := decide(g, b, 1, domain.ActionBuy, 0.99, at(11, 25)); v.Reason != ReasonRotation {
		t.Fatalf("verdict %+v, want a rotation once the hour is up", v)
	}
}

func TestEntryRulesCanBeTurnedOff(t *testing.T) {
	g := holdGate(t, func(l *Limits) { l.EntryMinProbability, l.EntryConfirmations = 0, 0 })
	b := NewBook()
	b.Mark(1, 1_000)
	if v := decide(g, b, 1, domain.ActionBuy, 0.61, at(10, 0)); !v.Approved() {
		t.Fatalf("with the entry rules off a 0.61 BUY was refused: %+v", v)
	}
}

func TestValidateEntryRules(t *testing.T) {
	for name, edit := range map[string]func(*Limits){
		"entry threshold above one": func(l *Limits) { l.EntryMinProbability = 1.1 },
		"negative entry threshold":  func(l *Limits) { l.EntryMinProbability = -0.1 },
		"negative confirmations":    func(l *Limits) { l.EntryConfirmations = -1 },
	} {
		l := DefaultLimits()
		edit(&l)
		if l.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
