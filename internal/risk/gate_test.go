package risk

import (
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// day is a Monday; times below are IST on that day.
var day = time.Date(2026, 10, 5, 0, 0, 0, 0, IST)

func at(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func inst(token uint32) domain.Instrument {
	return domain.Instrument{Token: token, Exchange: "NSE", Symbol: "SYM"}
}

func decision(a domain.Action, size string, p float64) domain.Decision {
	return domain.Decision{Action: a, Size: size, Probability: p}
}

func req(token uint32, d domain.Decision) Request {
	return Request{CycleID: "cycle-1", Instrument: inst(token), Decision: d}
}

// testLimits are DefaultLimits with a budget large enough that the hard caps
// (50,000 a position, 200,000 gross) bind, and size buckets of 10,000, 25,000
// and 50,000. The budget tests below use the default budget instead.
func testLimits() Limits {
	l := DefaultLimits()
	l.CapitalBudget = 250_000
	l.SizeFraction = map[string]float64{domain.SizeS: 0.04, domain.SizeM: 0.10, domain.SizeL: 0.20}
	l.MinHold, l.ReentryCooldown, l.CostAware = 0, 0, false // churn_test.go covers these
	return l
}

func newGate(t *testing.T, edit func(*Limits)) *Gate {
	t.Helper()
	l := testLimits()
	if edit != nil {
		edit(&l)
	}
	g, err := NewGate(l)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return g
}

func long(b *Book, token uint32, qty int64, avg, mark float64) {
	b.SetPosition(domain.Position{Instrument: inst(token), Quantity: qty, AvgPrice: avg})
	b.Mark(token, mark)
}

func TestDefaultLimitsAreValid(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewGateRejectsInvalidLimits(t *testing.T) {
	cases := map[string]func(*Limits){
		"missing buy threshold":       func(l *Limits) { delete(l.MinProbability, domain.ActionBuy) },
		"threshold above one":         func(l *Limits) { l.MinProbability[domain.ActionSell] = 1.5 },
		"missing size bucket":         func(l *Limits) { delete(l.SizeFraction, domain.SizeL) },
		"NaN size fraction":           func(l *Limits) { l.SizeFraction[domain.SizeS] = math.NaN() },
		"size fraction above one":     func(l *Limits) { l.SizeFraction[domain.SizeM] = 1.5 },
		"zero capital budget":         func(l *Limits) { l.CapitalBudget = 0 },
		"NaN capital budget":          func(l *Limits) { l.CapitalBudget = math.NaN() },
		"negative cost reserve":       func(l *Limits) { l.CostReserveRate = -0.01 },
		"NaN cost reserve":            func(l *Limits) { l.CostReserveRate = math.NaN() },
		"position fraction above one": func(l *Limits) { l.MaxPositionFraction = 2 },
		"rotation margin of one":      func(l *Limits) { l.RotationMargin = 1 },
		"negative minimum hold":       func(l *Limits) { l.RotationMinHold = -time.Second },
		"negative decider min hold":   func(l *Limits) { l.MinHold = -time.Second },
		"negative reentry cooldown":   func(l *Limits) { l.ReentryCooldown = -time.Second },
		"negative cost margin":        func(l *Limits) { l.CostMargin = -0.001 },
		"cost margin of ten percent":  func(l *Limits) { l.CostMargin = 0.1 },
		"cost check without a window": func(l *Limits) { l.CostAware = true; l.RangeWindow = 0 },
		"zero position cap":           func(l *Limits) { l.MaxPositionNotional = 0 },
		"gross below position cap":    func(l *Limits) { l.MaxGrossNotional = l.MaxPositionNotional - 1 },
		"zero daily loss":             func(l *Limits) { l.MaxDailyLoss = 0 },
		"per second at Kite limit":    func(l *Limits) { l.MaxOrdersPerSecond = 10 },
		"per minute at Kite limit":    func(l *Limits) { l.MaxOrdersPerMinute = 400 },
		"per day at Kite limit":       func(l *Limits) { l.MaxOrdersPerDay = 5000 },
		"no rejection budget":         func(l *Limits) { l.MaxConsecutiveRejections = 0 },
		"entry window inverted":       func(l *Limits) { l.EntryStart, l.EntryEnd = l.EntryEnd, l.EntryStart },
		"entries after square-off":    func(l *Limits) { l.EntryEnd = TimeOfDay{15, 15} },
		"invalid time of day":         func(l *Limits) { l.SquareOff = TimeOfDay{24, 0} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			l := DefaultLimits()
			edit(&l)
			if _, err := NewGate(l); err == nil {
				t.Fatal("NewGate accepted invalid limits")
			}
		})
	}
}

func TestGateCopiesLimits(t *testing.T) {
	l := DefaultLimits()
	g, err := NewGate(l)
	if err != nil {
		t.Fatal(err)
	}
	l.MinProbability[domain.ActionBuy] = 0.01
	g.Limits().SizeFraction[domain.SizeM] = 1
	if got := g.Limits().MinProbability[domain.ActionBuy]; got != 0.60 {
		t.Fatalf("caller's map edit leaked into the gate: threshold %v", got)
	}
	if got := g.Limits().SizeFraction[domain.SizeM]; got != 0.10 {
		t.Fatalf("Limits() returned a shared map: size %v", got)
	}
}

func TestEvaluateApprovesEntry(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	b.Mark(1, 500)
	now := at(10, 0)

	v := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeM, 0.7)), b, now)
	a, ok := v.Approval()
	if !ok || !v.Approved() || v.Reason != ReasonApproved {
		t.Fatalf("verdict = %+v, want approved", v)
	}
	want := domain.OrderIntent{CycleID: "cycle-1", Instrument: inst(1), Side: domain.ActionBuy, Quantity: 50, RefPrice: 500}
	if got := a.Intent(); got != want {
		t.Fatalf("intent = %+v, want %+v", got, want)
	}
	if a.Source() != SourceDecision || a.Reason() != ReasonApproved || !a.ApprovedAt().Equal(now) {
		t.Fatalf("approval metadata = %v %v %v", a.Source(), a.Reason(), a.ApprovedAt())
	}
	if !b.InFlight(1) {
		t.Fatal("approved order not recorded as in flight")
	}
}

func TestEvaluateRefusals(t *testing.T) {
	buyM := decision(domain.ActionBuy, domain.SizeM, 0.7)
	cases := []struct {
		name  string
		edit  func(*Limits)
		setup func(*Gate, *Book)
		d     domain.Decision
		now   time.Time
		want  Reason
	}{
		{name: "hold", d: domain.Decision{Action: domain.ActionHold, HoldReason: "timeout"}, want: ReasonHold},
		{name: "unknown action", d: decision("SHORT", domain.SizeM, 0.9), want: ReasonInvalidDecision},
		{name: "NaN probability", d: decision(domain.ActionBuy, domain.SizeM, math.NaN()), want: ReasonInvalidDecision},
		{name: "probability above one", d: decision(domain.ActionBuy, domain.SizeM, 1.2), want: ReasonInvalidDecision},
		{name: "unknown size", d: decision(domain.ActionBuy, "XL", 0.9), want: ReasonInvalidDecision},
		{
			name:  "kill switch",
			setup: func(_ *Gate, b *Book) { b.Kill(ReasonKillFile) },
			d:     buyM, want: ReasonKillSwitch,
		},
		{name: "before entry window", d: buyM, now: at(9, 19), want: ReasonOutsideSession},
		{name: "at entry end", d: buyM, now: at(15, 0), want: ReasonOutsideSession},
		{
			name:  "daily loss limit",
			setup: func(_ *Gate, b *Book) { long(b, 2, 100, 500, 440) }, // unrealized -6000
			d:     buyM, want: ReasonDailyLoss,
		},
		{name: "below threshold", d: decision(domain.ActionBuy, domain.SizeM, 0.59), want: ReasonBelowThreshold},
		{
			name: "order in flight",
			setup: func(g *Gate, b *Book) {
				g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(9, 59))
			},
			d: buyM, want: ReasonOrderInFlight,
		},
		{name: "short not allowed", d: decision(domain.ActionSell, domain.SizeM, 0.9), want: ReasonShortNotAllowed},
		{
			name:  "no price",
			setup: func(_ *Gate, b *Book) { delete(b.marks, 1) },
			d:     buyM, want: ReasonNoPrice,
		},
		{
			name:  "position cap",
			setup: func(_ *Gate, b *Book) { long(b, 1, 100, 500, 500) }, // 50,000 held: no room for a share
			d:     decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonPositionCap,
		},
		{
			name:  "one share over the position cap",
			setup: func(_ *Gate, b *Book) { b.Mark(1, 60_000) },
			d:     decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonPositionCap,
		},
		{
			name: "gross cap",
			setup: func(_ *Gate, b *Book) {
				for tok := uint32(10); tok < 14; tok++ {
					long(b, tok, 100, 500, 500) // 4 × 50,000 = 200,000, none free
				}
			},
			d: decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonGrossCap,
		},
		{
			name: "orders per second",
			setup: func(g *Gate, b *Book) {
				for tok := uint32(10); tok < 13; tok++ {
					b.Mark(tok, 100)
					g.Evaluate(req(tok, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(10, 0))
				}
			},
			d: decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonOrderRate,
		},
		{
			name: "orders per minute",
			edit: func(l *Limits) { l.MaxOrdersPerSecond, l.MaxOrdersPerMinute = 1, 2 },
			setup: func(g *Gate, b *Book) {
				for i, tok := range []uint32{10, 11} {
					b.Mark(tok, 100)
					g.Evaluate(req(tok, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(9, 59).Add(time.Duration(30+i*15)*time.Second))
				}
			},
			d: decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonOrderRate,
		},
		{
			name: "daily order budget",
			edit: func(l *Limits) { l.MaxOrdersPerSecond, l.MaxOrdersPerMinute, l.MaxOrdersPerDay = 1, 1, 2 },
			setup: func(g *Gate, b *Book) {
				for i, tok := range []uint32{10, 11} {
					b.Mark(tok, 100)
					g.Evaluate(req(tok, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(9, 30+i*10))
				}
			},
			d: decision(domain.ActionBuy, domain.SizeS, 0.9), want: ReasonDailyOrderBudget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(t, tc.edit)
			b := NewBook()
			b.Mark(1, 500)
			if tc.setup != nil {
				tc.setup(g, b)
			}
			now := tc.now
			if now.IsZero() {
				now = at(10, 0)
			}
			v := g.Evaluate(req(1, tc.d), b, now)
			if v.Reason != tc.want || v.Approved() {
				t.Fatalf("verdict = %+v, want refused with %s", v, tc.want)
			}
			if _, ok := v.Approval(); ok {
				t.Fatal("refused verdict carries an approval")
			}
		})
	}
}

func TestEvaluateCheckOrder(t *testing.T) {
	// Every check fails at once: the kill switch, checked first, decides.
	g := newGate(t, nil)
	b := NewBook()
	long(b, 2, 100, 500, 400)
	b.Kill(ReasonKillSignal)
	v := g.Evaluate(req(1, decision(domain.ActionSell, "XL", 0.1)), b, at(8, 0))
	if v.Reason != ReasonKillSwitch {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonKillSwitch)
	}
}

func TestReducingOrderClosesWholePosition(t *testing.T) {
	g := newGate(t, nil)
	for name, now := range map[string]time.Time{"in session": at(10, 0), "after entry end": at(15, 5)} {
		t.Run(name, func(t *testing.T) {
			b := NewBook()
			long(b, 1, 40, 500, 400) // also breaches the daily loss limit
			v := g.Evaluate(req(1, decision(domain.ActionSell, domain.SizeS, 0.8)), b, now)
			a, ok := v.Approval()
			if !ok {
				t.Fatalf("reducing SELL refused: %+v", v)
			}
			in := a.Intent()
			if in.Side != domain.ActionSell || in.Quantity != 40 || !in.ReduceOnly {
				t.Fatalf("intent = %+v, want reduce-only SELL 40", in)
			}
		})
	}
}

func TestReducingOrderStillNeedsThreshold(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	long(b, 1, 40, 500, 500)
	if v := g.Evaluate(req(1, decision(domain.ActionSell, domain.SizeS, 0.5)), b, at(10, 0)); v.Reason != ReasonBelowThreshold {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonBelowThreshold)
	}
}

func TestShortEntryWhenAllowed(t *testing.T) {
	g := newGate(t, func(l *Limits) { l.AllowShort = true })
	b := NewBook()
	b.Mark(1, 250)
	a, ok := g.Evaluate(req(1, decision(domain.ActionSell, domain.SizeS, 0.8)), b, at(10, 0)).Approval()
	if !ok {
		t.Fatal("short entry refused with AllowShort")
	}
	if in := a.Intent(); in.Side != domain.ActionSell || in.Quantity != 40 || in.ReduceOnly {
		t.Fatalf("intent = %+v, want SELL 40 entry", in)
	}
}

func TestZeroApprovalIsInvalid(t *testing.T) {
	var a Approval
	if a.Valid() {
		t.Fatal("zero Approval is valid")
	}
	if a.Intent() != (domain.OrderIntent{}) || a.Source() != "" || a.Reason() != "" || !a.ApprovedAt().IsZero() {
		t.Fatal("zero Approval exposes non-zero data")
	}
	if (Verdict{Reason: ReasonApproved}).Approved() {
		t.Fatal("a Verdict built outside the gate reports approved")
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	run := func() []Verdict {
		g := newGate(t, nil)
		b := NewBook()
		var out []Verdict
		for i := range 20 {
			tok := uint32(i % 4)
			b.Mark(tok, 100+float64(i))
			d := decision([]domain.Action{domain.ActionBuy, domain.ActionSell, domain.ActionHold}[i%3], domain.SizeS, 0.55+float64(i%5)*0.05)
			out = append(out, g.Evaluate(req(tok, d), b, at(10, i)))
			if i%7 == 0 {
				b.Settle(tok)
			}
		}
		return out
	}
	if a, b := run(), run(); !reflect.DeepEqual(a, b) {
		t.Fatal("identical inputs produced different verdicts")
	}
}

func TestExitsAtSquareOff(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	long(b, 2, 10, 100, 101)
	b.SetPosition(domain.Position{Instrument: inst(1), Quantity: -5, AvgPrice: 200})

	if plan := g.Exits(b, at(15, 9)); plan.Reason != "" || len(plan.Approvals) != 0 {
		t.Fatalf("exits before square-off: %+v", plan)
	}
	now := at(15, 10)
	plan := g.Exits(b, now)
	if plan.Reason != ReasonSquareOff || len(plan.Approvals) != 2 {
		t.Fatalf("plan = %+v, want two square-off exits", plan)
	}
	want := []domain.OrderIntent{
		{CycleID: "risk-square_off-1-" + strconv.FormatInt(now.UnixNano(), 10), Instrument: inst(1), Side: domain.ActionBuy, Quantity: 5, RefPrice: 200, ReduceOnly: true},
		{CycleID: "risk-square_off-2-" + strconv.FormatInt(now.UnixNano(), 10), Instrument: inst(2), Side: domain.ActionSell, Quantity: 10, RefPrice: 101, ReduceOnly: true},
	}
	for i, a := range plan.Approvals {
		if a.Intent() != want[i] || a.Source() != SourceRisk || a.Reason() != ReasonSquareOff {
			t.Fatalf("exit %d = %+v %s %s, want %+v", i, a.Intent(), a.Source(), a.Reason(), want[i])
		}
	}
	if again := g.Exits(b, now.Add(time.Second)); len(again.Approvals) != 0 {
		t.Fatalf("exits repeated while orders are in flight: %+v", again)
	}
}

func TestExitsRespectOrderRateButNotDailyBudget(t *testing.T) {
	g := newGate(t, func(l *Limits) { l.MaxOrdersPerSecond, l.MaxOrdersPerMinute, l.MaxOrdersPerDay = 2, 4, 4 })
	b := NewBook()
	b.dayOrders = 4 // daily budget exhausted
	for tok := uint32(1); tok <= 5; tok++ {
		long(b, tok, 1, 100, 100)
	}
	now := at(15, 10)
	counts := []int{2, 2, 0, 1}
	times := []time.Time{now, now.Add(time.Second), now.Add(2 * time.Second), now.Add(61 * time.Second)}
	for i, ts := range times {
		if got := len(g.Exits(b, ts).Approvals); got != counts[i] {
			t.Fatalf("call %d: %d exits, want %d", i, got, counts[i])
		}
	}
}

func TestExitsOnKillSwitchCancelEntries(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	long(b, 1, 10, 100, 100)
	b.Mark(2, 100)
	if !g.Evaluate(req(2, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(10, 0)).Approved() {
		t.Fatal("setup entry refused")
	}
	b.Kill(ReasonKillSignal)
	plan := g.Exits(b, at(10, 1))
	if plan.Reason != ReasonKillSwitch || len(plan.Approvals) != 1 || plan.Approvals[0].Intent().Instrument.Token != 1 {
		t.Fatalf("plan = %+v, want one kill-switch exit for token 1", plan)
	}
	if len(plan.Cancel) != 1 || plan.Cancel[0].Instrument.Token != 2 {
		t.Fatalf("cancel = %+v, want the in-flight entry for token 2", plan.Cancel)
	}
}

func TestExitsOnDailyLoss(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	long(b, 1, 100, 500, 460) // -4,000 unrealized
	if plan := g.Exits(b, at(11, 0)); plan.Reason != "" {
		t.Fatalf("exit before loss limit: %+v", plan)
	}
	b.Mark(1, 449) // -5,100
	plan := g.Exits(b, at(11, 1))
	if plan.Reason != ReasonDailyLoss || len(plan.Approvals) != 1 {
		t.Fatalf("plan = %+v, want one daily-loss exit", plan)
	}
	// The halt stays latched after the price recovers.
	b.Mark(1, 520)
	b.Mark(3, 100)
	if v := g.Evaluate(req(3, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(11, 2)); v.Reason != ReasonDailyLoss {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonDailyLoss)
	}
}

func TestApplyFill(t *testing.T) {
	b := NewBook()
	fill := func(side domain.Action, qty int64, price, charges float64) {
		b.ApplyFill(domain.Fill{Instrument: inst(1), Side: side, Quantity: qty, Price: price, Charges: charges})
	}
	fill(domain.ActionBuy, 10, 100, 0)
	fill(domain.ActionBuy, 10, 110, 0)
	if p, _ := b.Position(1); p.Quantity != 20 || p.AvgPrice != 105 {
		t.Fatalf("position = %+v, want 20 @ 105", p)
	}
	fill(domain.ActionSell, 5, 120, 2)
	if p, _ := b.Position(1); p.Quantity != 15 || p.AvgPrice != 105 || b.RealizedPnL() != 73 {
		t.Fatalf("position = %+v realized %v, want 15 @ 105 and 73", p, b.RealizedPnL())
	}
	fill(domain.ActionSell, 25, 100, 0) // closes 15 at -5 each, opens 10 short
	if p, _ := b.Position(1); p.Quantity != -10 || p.AvgPrice != 100 || b.RealizedPnL() != -2 {
		t.Fatalf("position = %+v realized %v, want -10 @ 100 and -2", p, b.RealizedPnL())
	}
	b.Mark(1, 90)
	if got := b.PnL(); got != 98 {
		t.Fatalf("PnL = %v, want 98", got)
	}
	fill(domain.ActionBuy, 10, 90, 0)
	if _, ok := b.Position(1); ok || len(b.Positions()) != 0 {
		t.Fatal("flat position still listed")
	}
}

func TestFillsDrainInFlightOrder(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	b.Mark(1, 500)
	a, _ := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeM, 0.9)), b, at(10, 0)).Approval()
	f := domain.Fill{Instrument: inst(1), Side: domain.ActionBuy, Quantity: 20, Price: 500}
	b.ApplyFill(f)
	if !b.InFlight(1) || b.inflight[1].Quantity != a.Intent().Quantity-20 {
		t.Fatalf("partial fill: in flight %+v", b.inflight[1])
	}
	f.Quantity = a.Intent().Quantity - 20
	b.ApplyFill(f)
	if b.InFlight(1) {
		t.Fatal("fully filled order still in flight")
	}
}

func TestDayRollover(t *testing.T) {
	g := newGate(t, func(l *Limits) { l.MaxOrdersPerSecond, l.MaxOrdersPerMinute, l.MaxOrdersPerDay = 1, 1, 1 })
	b := NewBook()
	b.Mark(1, 100)
	b.ApplyFill(domain.Fill{Instrument: inst(1), Side: domain.ActionBuy, Quantity: 100, Price: 200})
	b.ApplyFill(domain.Fill{Instrument: inst(1), Side: domain.ActionSell, Quantity: 100, Price: 140}) // -6,000 realized
	if v := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, at(10, 0)); v.Reason != ReasonDailyLoss {
		t.Fatalf("day 1 reason = %s, want %s", v.Reason, ReasonDailyLoss)
	}
	g.NoteRejection(b)

	next := at(10, 0).AddDate(0, 0, 1)
	if v := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, next); !v.Approved() {
		t.Fatalf("day 2 entry refused: %+v", v)
	}
	if b.RealizedPnL() != 0 || b.rejections != 0 {
		t.Fatalf("day state not reset: realized %v rejections %d", b.RealizedPnL(), b.rejections)
	}
	b.Settle(1)
	if v := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, next.Add(time.Hour)); v.Reason != ReasonDailyOrderBudget {
		t.Fatalf("reason = %s, want %s", v.Reason, ReasonDailyOrderBudget)
	}

	b.Kill(ReasonKillFile)
	b.Settle(1)
	if v := g.Evaluate(req(1, decision(domain.ActionBuy, domain.SizeS, 0.9)), b, next.AddDate(0, 0, 1)); v.Reason != ReasonKillSwitch {
		t.Fatalf("kill switch cleared by day rollover: %+v", v)
	}
}

func TestNoteRejectionTripsKillSwitch(t *testing.T) {
	g := newGate(t, nil)
	b := NewBook()
	g.NoteRejection(b)
	g.NoteRejection(b)
	b.NoteAccepted()
	g.NoteRejection(b)
	g.NoteRejection(b)
	if killed, _ := b.Killed(); killed {
		t.Fatal("kill switch tripped before three consecutive rejections")
	}
	g.NoteRejection(b)
	if killed, reason := b.Killed(); !killed || reason != ReasonBrokerRejections {
		t.Fatalf("killed = %v %s, want tripped by broker rejections", killed, reason)
	}
	b.Kill(ReasonIPWhitelist)
	if _, reason := b.Killed(); reason != ReasonBrokerRejections {
		t.Fatalf("second Kill replaced the first reason: %s", reason)
	}
}

func TestNextSquareOff(t *testing.T) {
	g := newGate(t, nil)
	if got := g.NextSquareOff(at(9, 0)); !got.Equal(at(15, 10)) {
		t.Fatalf("morning: %v", got)
	}
	if got := g.NextSquareOff(at(15, 10)); !got.Equal(at(15, 10)) {
		t.Fatalf("at square-off: %v", got)
	}
	if got := g.NextSquareOff(at(16, 0)); !got.Equal(at(15, 10).AddDate(0, 0, 1)) {
		t.Fatalf("evening: %v", got)
	}
	// A UTC timestamp is judged in IST: 09:45 UTC is 15:15 IST.
	utc := time.Date(2026, 10, 5, 9, 45, 0, 0, time.UTC)
	if plan := g.Exits(bookWith(1), utc); plan.Reason != ReasonSquareOff {
		t.Fatalf("UTC time not converted to IST: %+v", plan)
	}
}

func bookWith(token uint32) *Book {
	b := NewBook()
	long(b, token, 1, 100, 100)
	return b
}
