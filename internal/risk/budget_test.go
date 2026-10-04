package risk

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// budgetGate is a gate with the default 25,000 budget.
func budgetGate(t *testing.T, edit func(*Limits)) *Gate {
	t.Helper()
	l := DefaultLimits()
	l.MinHold, l.ReentryCooldown, l.CostAware, l.EntryConfirmations, l.EntryMinProbability, l.StopLossPct = 0, 0, false, 0, 0, 0 // the budget tests trade freely; churn_test.go covers these
	l.RotationMinHold = 5 * time.Minute                                                                                          // the budget tests rotate after minutes, not an hour
	if edit != nil {
		edit(&l)
	}
	g, err := NewGate(l)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return g
}

// fill books the approved order as completely filled at its reference price.
func fill(b *Book, a Approval) {
	in := a.Intent()
	b.ApplyFill(domain.Fill{Instrument: in.Instrument, Side: in.Side, Quantity: in.Quantity, Price: in.RefPrice})
	b.Settle(in.Instrument.Token)
}

func buy(g *Gate, b *Book, token uint32, price float64, size string, p float64, now time.Time) Verdict {
	b.Mark(token, price)
	return g.Evaluate(Request{CycleID: "c-" + now.Format("150405") + "-" + inst(token).Symbol + string(rune('a'+token)), Instrument: inst(token), Decision: decision(domain.ActionBuy, size, p)}, b, now)
}

func TestDefaultBudgetIs25000(t *testing.T) {
	if got := DefaultLimits().CapitalBudget; got != 25_000 {
		t.Fatalf("default capital budget = %v, want 25000", got)
	}
}

func TestEntryIsSizedFromTheBudget(t *testing.T) {
	g := budgetGate(t, nil)
	for _, tc := range []struct {
		size string
		want int64
	}{{domain.SizeS, 12}, {domain.SizeM, 25}, {domain.SizeL, 50}} { // 5%, 10%, 20% of 25,000 at 100
		b := NewBook()
		a, ok := buy(g, b, 1, 100, tc.size, 0.8, at(10, 0)).Approval()
		if !ok || a.Intent().Quantity != tc.want {
			t.Errorf("size %s: quantity %d (approved %v), want %d", tc.size, a.Intent().Quantity, ok, tc.want)
		}
	}
}

func TestEntryIsAtLeastOneShare(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	// S is 1,250; a share at 4,000 is more than the bucket but fits the budget and the position cap.
	a, ok := buy(g, b, 1, 4_000, domain.SizeS, 0.8, at(10, 0)).Approval()
	if !ok || a.Intent().Quantity != 1 {
		t.Fatalf("approved = %v quantity %d, want one share", ok, a.Intent().Quantity)
	}
}

func TestEntryIsClippedToFreeCapital(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	long(b, 2, 100, 100, 100) // 10,000 committed
	long(b, 3, 100, 100, 100) // 20,000 committed; budget 25,000 / 1.0015 leaves about 4,963
	v := buy(g, b, 1, 100, domain.SizeL, 0.8, at(10, 0))
	a, ok := v.Approval()
	if !ok || a.Intent().Quantity != 49 {
		t.Fatalf("verdict %+v quantity %d, want 49 shares clipped to the free capital", v, a.Intent().Quantity)
	}
	if !strings.Contains(v.Detail, "sized to free capital") {
		t.Errorf("detail %q does not say the entry was clipped", v.Detail)
	}
}

func TestEntryIsRefusedWhenOneShareDoesNotFit(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	long(b, 2, 240, 100, 100) // 24,000 committed; about 963 free
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.8, at(10, 0))
	if v.Reason != ReasonBudgetFull || v.Approved() {
		t.Fatalf("verdict %+v, want refused with %s", v, ReasonBudgetFull)
	}
	for _, want := range []string{"committed 24000.00", "budget 25000.00", "one share at 1000.00 does not fit"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail %q is missing %q", v.Detail, want)
		}
	}
}

func TestBudgetHoldsBackEstimatedCosts(t *testing.T) {
	g := budgetGate(t, func(l *Limits) { l.CostReserveRate = 0.01; l.MaxPositionFraction = 1 })
	b := NewBook()
	// 25,000 / 1.01 = 24,752: 24,800 would fit the raw budget but not after costs.
	if v := buy(g, b, 1, 24_800, domain.SizeL, 0.8, at(10, 0)); v.Approved() {
		t.Fatalf("a share costing %v fits a 25,000 budget only without its costs: %+v", 24_800, v)
	}
	if v := buy(g, b, 1, 24_700, domain.SizeL, 0.8, at(10, 1)); !v.Approved() {
		t.Fatalf("a share at 24,700 fits after costs: %+v", v)
	}
}

func TestOpenBuyOrdersCountAgainstTheBudget(t *testing.T) {
	g := budgetGate(t, func(l *Limits) { l.MaxPositionFraction = 1 })
	b := NewBook()
	if v := buy(g, b, 1, 100, domain.SizeL, 0.8, at(10, 0)); !v.Approved() { // 50 shares in flight, 5,000
		t.Fatalf("first entry refused: %+v", v)
	}
	if got := b.Committed(); got != 5_000 {
		t.Fatalf("committed = %v, want the 5,000 in flight", got)
	}
	// Four more L entries use the rest; the order for the sixth does not fit.
	var approved int
	for tok := uint32(2); tok < 12; tok++ {
		if buy(g, b, tok, 100, domain.SizeL, 0.8, at(10, int(tok))).Approved() {
			approved++
		}
	}
	if committed := b.Committed(); committed > g.Limits().Budget()+1e-9 {
		t.Fatalf("committed %.2f over the budget %.2f", committed, g.Limits().Budget())
	}
	if approved == 0 || approved == 10 {
		t.Fatalf("approved %d of 10 further entries, want the budget to stop some", approved)
	}
}

func TestSellFreesCapitalOnlyWhenFilled(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	long(b, 2, 240, 100, 100)
	sell := g.Evaluate(req(2, decision(domain.ActionSell, domain.SizeS, 0.8)), b, at(10, 0))
	a, ok := sell.Approval()
	if !ok {
		t.Fatalf("sell refused: %+v", sell)
	}
	if v := buy(g, b, 1, 1_000, domain.SizeS, 0.8, at(10, 1)); v.Approved() {
		t.Fatal("the capital was spent again before the sale filled")
	}
	fill(b, a)
	if v := buy(g, b, 1, 1_000, domain.SizeS, 0.8, at(10, 2)); !v.Approved() {
		t.Fatalf("the capital was not free after the sale filled: %+v", v)
	}
}

func TestManyRoundTripsPerStockPerDay(t *testing.T) {
	g := budgetGate(t, func(l *Limits) { l.MaxOrdersPerDay = 500 })
	b := NewBook()
	b.Mark(1, 100)
	now := at(9, 30)
	for i := range 6 {
		now = now.Add(30 * time.Second)
		a, ok := g.Evaluate(Request{CycleID: "buy-" + string(rune('a'+i)), Instrument: inst(1), Decision: decision(domain.ActionBuy, domain.SizeS, 0.8)}, b, now).Approval()
		if !ok {
			t.Fatalf("round trip %d: buy refused", i)
		}
		fill(b, a)
		now = now.Add(30 * time.Second)
		a, ok = g.Evaluate(Request{CycleID: "sell-" + string(rune('a'+i)), Instrument: inst(1), Decision: decision(domain.ActionSell, domain.SizeS, 0.8)}, b, now).Approval()
		if !ok {
			t.Fatalf("round trip %d: sell refused", i)
		}
		fill(b, a)
	}
	if b.Committed() != 0 {
		t.Fatalf("committed %v after six flat round trips", b.Committed())
	}
}

func TestRoundTripsStayWithinTheOrderRateAndDailyLoss(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	b.Mark(1, 100)
	now := at(9, 30)
	var refused Reason
	for i := range 40 { // a buy and a sell every second, far past the per-second limit
		a, ok := g.Evaluate(Request{CycleID: "b" + string(rune('A'+i)), Instrument: inst(1), Decision: decision(domain.ActionBuy, domain.SizeS, 0.8)}, b, now).Approval()
		if !ok {
			refused = g.Evaluate(Request{CycleID: "x", Instrument: inst(1), Decision: decision(domain.ActionBuy, domain.SizeS, 0.8)}, b, now).Reason
			break
		}
		fill(b, a)
		a, ok = g.Evaluate(Request{CycleID: "s" + string(rune('A'+i)), Instrument: inst(1), Decision: decision(domain.ActionSell, domain.SizeS, 0.8)}, b, now).Approval()
		if ok {
			fill(b, a)
		}
	}
	if refused != ReasonOrderRate {
		t.Fatalf("refusal = %q, want %s: round trips must still obey the order-rate limit", refused, ReasonOrderRate)
	}

	// A day that has lost the limit takes no new entry.
	b = NewBook()
	b.ApplyFill(domain.Fill{Instrument: inst(1), Side: domain.ActionBuy, Quantity: 100, Price: 100})
	b.ApplyFill(domain.Fill{Instrument: inst(1), Side: domain.ActionSell, Quantity: 100, Price: 40}) // -6,000
	if v := buy(g, b, 2, 100, domain.SizeS, 0.8, at(11, 0)); v.Reason != ReasonDailyLoss {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonDailyLoss)
	}
}

// openBook returns a gate and a book that is full: five positions of 5,000
// opened at the given probabilities at 10:00, and 25,000 budget (24,962 after
// costs) all but spent.
func fullBook(t *testing.T, probs ...float64) (*Gate, *Book) {
	t.Helper()
	g := budgetGate(t, func(l *Limits) { l.MaxPositionFraction = 0.25 })
	b := NewBook()
	for i, p := range probs {
		tok := uint32(10 + i)
		v := buy(g, b, tok, 100, domain.SizeL, p, at(10, i*2)) // L is 5,000: 50 shares
		a, ok := v.Approval()
		if !ok {
			t.Fatalf("setup entry %d refused: %+v", i, v)
		}
		fill(b, a)
	}
	return g, b
}

func TestRotationSellsTheWeakestOwnPosition(t *testing.T) {
	g, b := fullBook(t, 0.70, 0.62, 0.75, 0.66, 0.90) // 25,000 committed
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.88, at(11, 0))
	rot, ok := v.Rotation()
	if !ok || v.Reason != ReasonRotation || v.Approved() {
		t.Fatalf("verdict %+v, want a rotation and no entry yet", v)
	}
	in := rot.Intent()
	if in.Instrument.Token != 11 || in.Side != domain.ActionSell || in.Quantity != 50 || !in.ReduceOnly {
		t.Fatalf("rotation intent = %+v, want a reduce-only SELL of all 50 shares of the weakest position (token 11, opened at 0.62)", in)
	}
	if rot.Source() != SourceRotation || rot.Reason() != ReasonRotation {
		t.Fatalf("rotation approval = %v %v", rot.Source(), rot.Reason())
	}
	if !b.InFlight(11) {
		t.Fatal("the rotation sale is not in flight")
	}
	if !strings.Contains(v.Detail, "to make room") {
		t.Errorf("detail %q does not explain the rotation", v.Detail)
	}

	// Once the sale fills the entry fits, and it is not rotated again.
	fill(b, rot)
	retry := Request{CycleID: "cycle-1-rot-buy", Instrument: inst(1), Decision: decision(domain.ActionBuy, domain.SizeS, 0.88), Rotated: true}
	if a, ok := g.Evaluate(retry, b, at(11, 1)).Approval(); !ok || a.Intent().Quantity != 1 {
		t.Fatalf("entry after rotation: approved %v quantity %d", ok, a.Intent().Quantity)
	}
}

func TestRotationNeedsAStrongerBuy(t *testing.T) {
	g, b := fullBook(t, 0.70, 0.75, 0.80, 0.85, 0.90)
	// 0.79 does not beat the weakest position (0.70) by the 0.10 margin.
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.79, at(11, 0))
	if v.Reason != ReasonBudgetFull {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonBudgetFull)
	}
	if _, ok := v.Rotation(); ok {
		t.Fatal("rotated for a BUY that is not stronger by the margin")
	}
}

func TestRotationRespectsMinimumHold(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62) // the last was opened at 10:08
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.9, at(10, 10))
	rot, ok := v.Rotation()
	if !ok {
		t.Fatalf("verdict %+v, want a rotation", v)
	}
	if tok := rot.Intent().Instrument.Token; tok != 10 { // opened 10:00, held 10 minutes; ties go to the lowest token
		t.Fatalf("rotated token %d, want 10", tok)
	}
	// At 10:03 only the positions opened by 09:58 are old enough: none.
	g, b = fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62)
	if v := buy(g, b, 1, 1_000, domain.SizeS, 0.9, at(10, 3)); v.Reason != ReasonBudgetFull {
		t.Fatalf("reason = %s, want %s: no position had been held long enough", v.Reason, ReasonBudgetFull)
	}
}

func TestRotationDoesNotHelpWhenTheSaleFreesTooLittle(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62) // 5,000 each
	if v := buy(g, b, 1, 6_000, domain.SizeS, 0.9, at(11, 0)); v.Reason != ReasonBudgetFull {
		t.Fatalf("reason = %s, want %s: selling 5,000 cannot make room for a 6,000 share", v.Reason, ReasonBudgetFull)
	}
}

func TestRotationOnlyTouchesPositionsOpenedToday(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62)
	// The next day the same positions are carried, not opened today.
	next := at(11, 0).AddDate(0, 0, 1)
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.9, next)
	if _, ok := v.Rotation(); ok {
		t.Fatal("rotated out a position opened on an earlier day")
	}
}

func TestRotationNeverTouchesPositionsWithoutAnEntryRecord(t *testing.T) {
	g := budgetGate(t, nil)
	b := NewBook()
	for tok := uint32(10); tok < 15; tok++ { // restored with SetPosition: not provably opened by this run today
		long(b, tok, 50, 100, 100)
	}
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.99, at(11, 0))
	if _, ok := v.Rotation(); ok || v.Reason != ReasonBudgetFull {
		t.Fatalf("verdict %+v, want budget_full and no rotation", v)
	}
}

func TestRotationSkipsPositionsWithOrdersInFlight(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.70, 0.70, 0.70, 0.70)
	sell := g.Evaluate(req(10, decision(domain.ActionSell, domain.SizeS, 0.8)), b, at(11, 0)) // the weakest is already being sold
	if !sell.Approved() {
		t.Fatalf("setup sell refused: %+v", sell)
	}
	v := buy(g, b, 1, 1_000, domain.SizeS, 0.9, at(11, 1))
	rot, ok := v.Rotation()
	if !ok || rot.Intent().Instrument.Token != 11 {
		t.Fatalf("verdict %+v, want rotation out of the next weakest, token 11", v)
	}
}

func TestRotatedRequestDoesNotRotateAgain(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62)
	b.Mark(1, 1_000)
	r := Request{CycleID: "c", Instrument: inst(1), Decision: decision(domain.ActionBuy, domain.SizeS, 0.95), Rotated: true}
	if v := g.Evaluate(r, b, at(11, 0)); v.Reason != ReasonBudgetFull {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonBudgetFull)
	}
}

func TestRotationCountsAgainstTheOrderLimits(t *testing.T) {
	g, b := fullBook(t, 0.62, 0.62, 0.62, 0.62, 0.62)
	for i := range g.Limits().MaxOrdersPerSecond { // use up this second's orders
		b.record(domain.OrderIntent{Instrument: inst(uint32(50 + i))}, at(11, 0))
	}
	if v := buy(g, b, 1, 1_000, domain.SizeS, 0.95, at(11, 0)); v.Reason != ReasonBudgetFull {
		t.Fatalf("reason = %s, want %s: a rotation sale is an order like any other", v.Reason, ReasonBudgetFull)
	}
	if got := b.dayOrders; got != 5 {
		t.Fatalf("dayOrders = %d, want only the 5 entries", got)
	}
}

// The invariant of issue #42: the bot never sells, modifies or otherwise
// touches a holding or position it did not open.
func TestForeignPositionsAreNeverTouched(t *testing.T) {
	g := budgetGate(t, func(l *Limits) { l.AllowShort = true })
	b := NewBook()
	foreign := inst(77)
	b.SetForeign(domain.Position{Instrument: foreign, Quantity: 500, AvgPrice: 200})
	b.Mark(77, 200)

	for _, a := range []domain.Action{domain.ActionBuy, domain.ActionSell} {
		for _, size := range []string{domain.SizeS, domain.SizeM, domain.SizeL} {
			v := g.Evaluate(Request{CycleID: "f", Instrument: foreign, Decision: decision(a, size, 0.99)}, b, at(10, 0))
			if v.Reason != ReasonForeignPosition || v.Approved() {
				t.Fatalf("%s %s on a foreign position: %+v", a, size, v)
			}
			if _, ok := v.Rotation(); ok {
				t.Fatal("a refused order carries a rotation")
			}
		}
	}
	// It is not in the book: no exit flattens it, not even at square-off, after a kill or a loss halt.
	b.Kill(ReasonKillFile)
	if plan := g.Exits(b, at(15, 30)); len(plan.Approvals) != 0 || len(plan.Cancel) != 0 {
		t.Fatalf("exit plan touches foreign holdings: %+v", plan)
	}
	if b.Committed() != 0 {
		t.Fatalf("foreign holding counted as committed capital: %v", b.Committed())
	}
	if !b.IsForeign(77) || b.IsForeign(1) {
		t.Fatal("IsForeign is wrong")
	}
}

func TestForeignInvariantHoldsUnderRandomDecisions(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	g := budgetGate(t, func(l *Limits) {
		l.AllowShort = true
		l.MaxOrdersPerSecond = 9
		l.MaxOrdersPerMinute = 399
		l.MaxOrdersPerDay = 4999
		l.RotationMinHold = 0
	})
	b := NewBook()
	foreign := map[uint32]bool{3: true, 7: true, 11: true}
	for tok := range foreign {
		b.SetForeign(domain.Position{Instrument: inst(tok), Quantity: 100, AvgPrice: 150})
	}
	budget := g.Limits().Budget()
	var now time.Time
	var approvals, rotations int
	for i := range 3_000 {
		now = at(9, 30).Add(time.Duration(i) * 5 * time.Second) // ends before 14:00
		tok := uint32(rng.Intn(15))
		price := float64(40+tok*30) * (1 + (rng.Float64()-0.5)/50) // steady prices: the loss limit must not end the run
		b.Mark(tok, price)
		act := []domain.Action{domain.ActionBuy, domain.ActionBuy, domain.ActionSell, domain.ActionHold}[rng.Intn(4)]
		size := []string{domain.SizeS, domain.SizeM, domain.SizeL}[rng.Intn(3)]
		v := g.Evaluate(Request{CycleID: "r" + string(rune(i)), Instrument: inst(tok), Decision: decision(act, size, 0.6+rng.Float64()*0.4)}, b, now)
		for _, a := range []Approval{approvalOf(v), rotationOf(v)} {
			if !a.Valid() {
				continue
			}
			approvals++
			if a.Source() == SourceRotation {
				rotations++
			}
			if foreign[a.Intent().Instrument.Token] {
				t.Fatalf("approval %+v touches foreign instrument", a.Intent())
			}
			if rng.Intn(10) > 0 { // most orders fill, some stay open
				fill(b, a)
			}
		}
		if rng.Intn(5) == 0 {
			b.Settle(uint32(rng.Intn(15)))
		}
		if got := b.Committed(); got > budget+1e-6 {
			t.Fatalf("step %d: committed %.2f over the budget %.2f", i, got, budget)
		}
		for _, p := range b.Positions() {
			if foreign[p.Instrument.Token] {
				t.Fatalf("foreign instrument %d became a bot position", p.Instrument.Token)
			}
		}
	}
	if approvals < 100 || rotations == 0 {
		t.Fatalf("%d approvals and %d rotations: the property test did not exercise the gate", approvals, rotations)
	}
}

func approvalOf(v Verdict) Approval { a, _ := v.Approval(); return a }
func rotationOf(v Verdict) Approval { a, _ := v.Rotation(); return a }
