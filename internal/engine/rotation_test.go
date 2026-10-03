package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

var (
	bankA   = domain.Instrument{Token: 11, Exchange: "NSE", Symbol: "AAA"}
	bankB   = domain.Instrument{Token: 12, Exchange: "NSE", Symbol: "BBB"}
	bankC   = domain.Instrument{Token: 13, Exchange: "NSE", Symbol: "CCC"}
	heldFor = domain.Instrument{Token: 14, Exchange: "NSE", Symbol: "HELD"} // held by the captain, not by the bot
)

// flatTicks is one tick a minute from 09:15 for n minutes at a steady price.
func flatTicks(in domain.Instrument, price float64, n int) []marketdata.Event {
	var evs []marketdata.Event
	for i := range n {
		t := tickAt(in, 9, 15+i, 30, price)
		evs = append(evs, marketdata.Event{Tick: &t})
	}
	return evs
}

// scripted answers by the instrument's alias (I01 = AAA, I02 = BBB, I03 = CCC,
// I04 = HELD, in order of first bar) and the minutes since the open.
type scripted map[string]map[int]domain.Decision

func (s scripted) Decide(_ context.Context, state json.RawMessage) (domain.Decision, error) {
	var snap struct {
		Instrument string `json:"instrument"`
		Session    struct {
			Minutes int `json:"minutes_since_open"`
		} `json:"session"`
	}
	if err := json.Unmarshal(state, &snap); err != nil {
		return domain.Decision{}, err
	}
	if d, ok := s[snap.Instrument][snap.Session.Minutes]; ok {
		return d, nil
	}
	return domain.Decision{Action: domain.ActionHold, Probabilities: map[domain.Action]float64{}}, nil
}

func buyAt(p float64) domain.Decision {
	return domain.Decision{Action: domain.ActionBuy, Size: domain.SizeS, Probability: p, Probabilities: map[domain.Action]float64{domain.ActionBuy: p, domain.ActionHold: 1 - p}}
}

func sellAt(p float64) domain.Decision {
	return domain.Decision{Action: domain.ActionSell, Size: domain.SizeS, Probability: p, Probabilities: map[domain.Action]float64{domain.ActionSell: p, domain.ActionHold: 1 - p}}
}

// A 3,000 budget holds two 1,000 shares. A third, stronger BUY sells the
// weakest position the bot opened, waits for the sale to fill, then buys. A
// holding the captain owns is never touched.
func TestFullBudgetRotatesOutTheWeakestOwnPosition(t *testing.T) {
	var evs []marketdata.Event
	a, b, c, h := flatTicks(bankA, 1000, 22), flatTicks(bankB, 1000, 22), flatTicks(bankC, 1000, 22), flatTicks(heldFor, 200, 22)
	for i := range a {
		evs = append(evs, a[i], b[i], c[i], h[i])
	}
	d := scripted{
		"I01": {8: buyAt(0.62)},
		"I02": {9: buyAt(0.65)},
		"I03": {16: buyAt(0.90)},
		"I04": {6: buyAt(0.99), 12: sellAt(0.99), 18: buyAt(0.99)},
	}
	r := newRig(t, evs, d)
	r.cfg.Gate = mustGate(t, func(l *risk.Limits) {
		l.CapitalBudget = 3_000
		l.MaxPositionFraction = 1
		l.RotationMinHold = 5 * time.Minute
	})
	r.cfg.Foreign = []domain.Position{{Instrument: heldFor, Quantity: 1000, AvgPrice: 150}}
	res := r.run(t)

	if res.Rotations != 1 {
		t.Fatalf("rotations = %d, want 1", res.Rotations)
	}
	held := map[uint32]int64{}
	for _, p := range res.Positions {
		held[p.Instrument.Token] = p.Quantity
	}
	if len(held) != 2 || held[bankB.Token] != 1 || held[bankC.Token] != 1 {
		t.Fatalf("open positions = %v, want one share each of BBB and CCC (AAA, the weakest, was rotated out)", held)
	}

	cs := cyclesOf(t, r.log, "run1")
	var sale, entry *decisionlog.Cycle
	var rotationVerdict string
	for i, cy := range cs {
		switch {
		case strings.HasSuffix(cy.CycleID, "-rot-buy"):
			entry = &cs[i]
		case strings.Contains(cy.CycleID, "-rot-"):
			sale = &cs[i]
		case cy.Verdict == string(risk.ReasonRotation):
			rotationVerdict = cy.VerdictDetail
		}
	}
	if sale == nil || entry == nil || rotationVerdict == "" {
		t.Fatalf("log has no rotation rows (sale %v, entry %v, verdict %q)", sale, entry, rotationVerdict)
	}
	if sale.Source != decisionlog.SourceRisk || sale.ExitReason != string(risk.ReasonRotation) ||
		sale.Intent == nil || sale.Intent.Instrument.Token != bankA.Token || sale.Intent.Side != domain.ActionSell || !sale.Intent.ReduceOnly {
		t.Errorf("sale row = %+v, want a reduce-only SELL of AAA logged as a rotation", sale)
	}
	if entry.Source != decisionlog.SourceDecision || entry.Snapshot != nil || entry.Verdict != string(risk.ReasonApproved) ||
		entry.Intent == nil || entry.Intent.Instrument.Token != bankC.Token || entry.Intent.Side != domain.ActionBuy {
		t.Errorf("entry row = %+v, want an approved BUY of CCC without a second snapshot", entry)
	}
	if !strings.Contains(rotationVerdict, "AAA") || !strings.Contains(rotationVerdict, "CCC") {
		t.Errorf("rotation detail %q should name both stocks", rotationVerdict)
	}
	// The sale was placed before the buy.
	var saleAt, buyAt int
	for i, ev := range eventsOf(t, r.log, "run1") {
		if ev.CycleID == sale.CycleID && saleAt == 0 {
			saleAt = i + 1
		}
		if ev.CycleID == entry.CycleID && buyAt == 0 {
			buyAt = i + 1
		}
	}
	if saleAt == 0 || buyAt == 0 || saleAt > buyAt {
		t.Errorf("sale events start at %d and buy events at %d; the buy must follow the sale", saleAt, buyAt)
	}

	// Nothing the bot did touched the captain's holding, though the decider asked for it.
	if n := countVerdict(cs, string(risk.ReasonForeignPosition)); n != 3 {
		t.Errorf("foreign_position refusals = %d, want the decider's 3 requests on the held stock", n)
	}
	for _, ev := range eventsOf(t, r.log, "run1") {
		if ev.Instrument.Token == heldFor.Token {
			t.Fatalf("an order event for the captain's holding: %+v", ev)
		}
	}
	// Four orders in all: three buys and the rotation sale.
	if held, _ := r.broker.Orders(bg); len(held) != 4 {
		t.Errorf("the broker holds %d orders, want 4 (3 buys and 1 sale)", len(held))
	}
}

// A BUY that is not stronger than every position by the margin finds the
// budget full and is refused, with nothing sold.
func TestFullBudgetWithoutAStrongerBuyIsRefused(t *testing.T) {
	var evs []marketdata.Event
	a, b, c := flatTicks(bankA, 1000, 22), flatTicks(bankB, 1000, 22), flatTicks(bankC, 1000, 22)
	for i := range a {
		evs = append(evs, a[i], b[i], c[i])
	}
	d := scripted{"I01": {8: buyAt(0.70)}, "I02": {9: buyAt(0.72)}, "I03": {16: buyAt(0.75)}}
	r := newRig(t, evs, d)
	r.cfg.Gate = mustGate(t, func(l *risk.Limits) { l.CapitalBudget = 3_000; l.MaxPositionFraction = 1 })
	res := r.run(t)

	if res.Rotations != 0 || len(res.Positions) != 2 {
		t.Fatalf("rotations %d, %d open positions; want no rotation and the two original positions", res.Rotations, len(res.Positions))
	}
	cs := cyclesOf(t, r.log, "run1")
	if n := countVerdict(cs, string(risk.ReasonBudgetFull)); n != 1 {
		t.Fatalf("budget_full refusals = %d, want 1", n)
	}
	for _, cy := range cs {
		if cy.Verdict == string(risk.ReasonBudgetFull) && !strings.Contains(cy.VerdictDetail, "one share at 1000.00 does not fit") {
			t.Errorf("refusal detail %q does not say why", cy.VerdictDetail)
		}
	}
}

func mustGate(t *testing.T, edit func(*risk.Limits)) *risk.Gate {
	t.Helper()
	l := risk.DefaultLimits()
	l.MinHold, l.ReentryCooldown, l.CostAware, l.EntryConfirmations, l.EntryMinProbability = 0, 0, false, 0, 0 // these tests are about rotation, not churn
	edit(&l)
	g, err := risk.NewGate(l)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
