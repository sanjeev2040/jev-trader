package orders_test

import (
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// Only the risk gate mints an Approval, so tests ask the real gate for one.
// A budget of 250,000 makes a size-M BUY at entryPrice entryQuantity shares.
const (
	entryPrice    = 250.0
	entryQuantity = 100
)

var infy = domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}

// t0 is inside the gate's entry window.
var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, risk.IST)

// approveSide has the gate approve a size-M entry for cycle on a fresh book.
func approveSide(t testing.TB, cycle string, side domain.Action) risk.Approval {
	t.Helper()
	l := risk.DefaultLimits()
	l.CapitalBudget = 250_000
	g, err := risk.NewGate(l)
	if err != nil {
		t.Fatal(err)
	}
	b := risk.NewBook()
	b.Mark(infy.Token, entryPrice)
	if side == domain.ActionSell {
		b.SetPosition(domain.Position{Instrument: infy, Quantity: entryQuantity, AvgPrice: entryPrice})
	}
	d := domain.Decision{Action: side, Size: domain.SizeM, Probability: 0.9}
	a, ok := g.Evaluate(risk.Request{CycleID: cycle, Instrument: infy, Decision: d}, b, t0).Approval()
	if !ok {
		t.Fatalf("gate refused %s %s", side, cycle)
	}
	return a
}

// approve returns a BUY approval: a limit order at entryPrice.
func approve(t testing.TB, cycle string) risk.Approval {
	t.Helper()
	return approveSide(t, cycle, domain.ActionBuy)
}

// approveExit returns a reduce-only SELL approval: a market order.
func approveExit(t testing.TB, cycle string) risk.Approval {
	t.Helper()
	return approveSide(t, cycle, domain.ActionSell)
}

func intentOf(t testing.TB, cycle string) domain.OrderIntent {
	t.Helper()
	return approve(t, cycle).Intent()
}
