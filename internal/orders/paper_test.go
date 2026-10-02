package orders_test

import (
	"context"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
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
	a := approve(t, "c1")
	o, err := m.Submit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b.OnTick(domain.Tick{InstrumentToken: infy.Token, LastPrice: 249, ExchangeTime: t0.Add(time.Second)})

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
	got, _ := m.Order(orders.ClientID(o.Intent.CycleID))
	if got.Status != orders.StatusFilled || got.FilledQuantity != entryQuantity || len(fills) != 1 {
		t.Fatalf("order %+v, fills %+v", got, fills)
	}
	// 2 bps on 249 rounds up to 249.05, inside the 250 limit.
	if fills[0].Price != 249.05 || fills[0].Charges <= 0 {
		t.Errorf("fill %+v", fills[0])
	}
	if _, err := m.Submit(ctx, a); err == nil {
		t.Error("filled cycle was submitted again")
	}
}
