package orders_test

import (
	"context"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

type clockAt struct{ now time.Time }

func (c clockAt) Now() time.Time { return c.now }

// TestManagerWithPaperBroker runs the paper-mode path the engine uses:
// reconcile, submit an approval, feed market data, and apply the updates.
func TestManagerWithPaperBroker(t *testing.T) {
	ctx := context.Background()
	b, err := paper.New(clockAt{t0}, paper.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	m, err := orders.NewManager(b, &memJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(ctx, nil); err != nil {
		t.Fatal(err)
	}
	a := risk.ApprovalForTest(t, limitIntent("c1"))
	o, err := m.Submit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b.OnTick(domain.Tick{Token: infy.Token, LastPrice: 1499, ExchangeTime: t0.Add(time.Second)})

	var fills []domain.Fill
	for len(b.Updates()) > 0 {
		_, f, err := m.Apply(<-b.Updates())
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			fills = append(fills, *f)
		}
	}
	got, _ := m.Order(o.Intent.ClientID)
	if got.Status != orders.StatusFilled || got.FilledQuantity != 100 || len(fills) != 1 {
		t.Fatalf("order %+v, fills %+v", got, fills)
	}
	// 2 bps on 1499 is 1499.30, inside the 1500 limit.
	if fills[0].Price != 1499.3 || fills[0].Costs <= 0 {
		t.Errorf("fill %+v", fills[0])
	}
	if _, err := m.Submit(ctx, a); err == nil {
		t.Error("filled cycle was submitted again")
	}
}
