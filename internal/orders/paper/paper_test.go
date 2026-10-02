package paper_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.FixedZone("IST", 19800))

var infy = domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func intent(cycle string, side domain.Action, typ domain.OrderType, limit float64) domain.OrderIntent {
	o := domain.OrderIntent{
		CycleID:    cycle,
		ClientID:   orders.ClientID(cycle),
		Instrument: infy,
		Side:       side,
		Quantity:   10,
		Type:       typ,
		LimitPrice: limit,
	}
	if typ == domain.OrderTypeMarket {
		o.MarketProtection = 2
	}
	return o
}

func newBroker(t *testing.T, cfg paper.Config) (*paper.Broker, *fixedClock) {
	t.Helper()
	clk := &fixedClock{now: t0}
	b, err := paper.New(clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b, clk
}

// drain returns every update already sent.
func drain(b *paper.Broker) []domain.OrderUpdate {
	var out []domain.OrderUpdate
	for {
		select {
		case u := <-b.Updates():
			out = append(out, u)
		default:
			return out
		}
	}
}

func tick(price float64, at time.Time) domain.Tick {
	return domain.Tick{Token: infy.Token, LastPrice: price, ExchangeTime: at}
}

func place(t *testing.T, b *paper.Broker, o domain.OrderIntent) string {
	t.Helper()
	id, err := b.Place(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := paper.New(nil, paper.DefaultConfig()); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := paper.New(&fixedClock{}, paper.Config{SlippageBps: -1}); err == nil {
		t.Error("negative slippage accepted")
	}
}

func TestPlaceAcksAndFillsOnFirstLaterTick(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	o := intent("c1", domain.ActionBuy, domain.OrderTypeMarket, 0)
	id := place(t, b, o)

	ack := drain(b)
	if len(ack) != 1 || ack[0].Status != domain.OrderStatusOpen || ack[0].ClientID != o.ClientID || ack[0].BrokerID != id {
		t.Fatalf("ack = %+v", ack)
	}
	// A tick at the order time, or on another instrument, does not fill.
	b.OnTick(tick(100, t0))
	b.OnTick(domain.Tick{Token: 1, LastPrice: 50, ExchangeTime: t0.Add(time.Second)})
	if u := drain(b); len(u) != 0 {
		t.Fatalf("filled early: %+v", u)
	}
	b.OnTick(tick(101, t0.Add(time.Second)))
	b.OnTick(tick(102, t0.Add(2*time.Second)))
	got := drain(b)
	if len(got) != 1 {
		t.Fatalf("updates = %+v, want one fill", got)
	}
	f := got[0]
	if f.Status != domain.OrderStatusFilled || f.FilledQuantity != 10 || f.AveragePrice != 101 || !f.Time.Equal(t0.Add(time.Second)) {
		t.Errorf("fill = %+v", f)
	}
	if want := (paper.CostModel{}).Charges(domain.ActionBuy, 10, 101); f.Costs != want {
		t.Errorf("costs = %v, want %v", f.Costs, want)
	}
}

func TestTickFallsBackToReceiveTime(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	place(t, b, intent("c1", domain.ActionBuy, domain.OrderTypeMarket, 0))
	drain(b)
	b.OnTick(domain.Tick{Token: infy.Token, LastPrice: 100, ReceivedAt: t0.Add(time.Millisecond)})
	if u := drain(b); len(u) != 1 || u[0].Status != domain.OrderStatusFilled {
		t.Fatalf("updates = %+v", u)
	}
}

func TestFillPrices(t *testing.T) {
	cases := []struct {
		name  string
		o     domain.OrderIntent
		price float64
		fills bool
		want  float64
	}{
		// 2 bps on 100 is 0.02, rounded to the 0.05 tick against the order.
		{"market buy slips up", intent("a", domain.ActionBuy, domain.OrderTypeMarket, 0), 100, true, 100.05},
		{"market sell slips down", intent("b", domain.ActionSell, domain.OrderTypeMarket, 0), 100, true, 99.95},
		{"buy limit caps slippage", intent("c", domain.ActionBuy, domain.OrderTypeLimit, 100), 100, true, 100},
		{"buy limit below market", intent("d", domain.ActionBuy, domain.OrderTypeLimit, 99), 100, false, 0},
		{"buy limit above market", intent("e", domain.ActionBuy, domain.OrderTypeLimit, 101), 100, true, 100.05},
		{"sell limit caps slippage", intent("f", domain.ActionSell, domain.OrderTypeLimit, 100), 100, true, 100},
		{"sell limit above market", intent("g", domain.ActionSell, domain.OrderTypeLimit, 101), 100, false, 0},
		{"slipped price rounds up a tick", intent("h", domain.ActionBuy, domain.OrderTypeMarket, 0), 99.95, true, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := newBroker(t, paper.Config{SlippageBps: 2})
			place(t, b, c.o)
			drain(b)
			b.OnTick(tick(c.price, t0.Add(time.Second)))
			got := drain(b)
			if !c.fills {
				if len(got) != 0 {
					t.Fatalf("filled: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].AveragePrice != c.want {
				t.Fatalf("updates = %+v, want fill at %v", got, c.want)
			}
		})
	}
}

func TestLimitOrderWaitsForPrice(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	place(t, b, intent("c1", domain.ActionBuy, domain.OrderTypeLimit, 99))
	drain(b)
	b.OnTick(tick(100, t0.Add(time.Second)))
	if u := drain(b); len(u) != 0 {
		t.Fatalf("filled above limit: %+v", u)
	}
	b.OnTick(tick(98.5, t0.Add(2*time.Second)))
	if u := drain(b); len(u) != 1 || u[0].AveragePrice != 98.5 {
		t.Fatalf("updates = %+v, want fill at 98.5", u)
	}
}

func TestBarFills(t *testing.T) {
	bar := func(start time.Time, o, h, l, c float64) domain.Bar {
		return domain.Bar{Instrument: infy, Interval: time.Minute, Start: start, End: start.Add(time.Minute), Open: o, High: h, Low: l, Close: c}
	}
	b, clk := newBroker(t, paper.Config{})
	clk.now = t0.Add(300 * time.Millisecond) // bar close + decision latency
	buy := place(t, b, intent("buy", domain.ActionBuy, domain.OrderTypeLimit, 99))
	sell := place(t, b, intent("sell", domain.ActionSell, domain.OrderTypeLimit, 101.5))
	mkt := place(t, b, intent("mkt", domain.ActionBuy, domain.OrderTypeMarket, 0))
	drain(b)

	// This bar started before the orders, so its range could predate them.
	b.OnBar(bar(t0, 100, 105, 90, 100))
	if u := drain(b); len(u) != 0 {
		t.Fatalf("filled from an earlier bar: %+v", u)
	}
	next := t0.Add(time.Minute)
	b.OnBar(bar(next, 100, 102, 98.5, 101))
	got := map[string]domain.OrderUpdate{}
	for _, u := range drain(b) {
		got[u.BrokerID] = u
	}
	for id, want := range map[string]float64{buy: 99, sell: 101.5, mkt: 100} {
		if u, ok := got[id]; !ok || u.AveragePrice != want || !u.Time.Equal(next.Add(time.Minute)) {
			t.Errorf("%s: update %+v, want fill at %v", id, u, want)
		}
	}
}

func TestCancel(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	ctx := context.Background()
	id := place(t, b, intent("c1", domain.ActionBuy, domain.OrderTypeLimit, 90))
	drain(b)
	if err := b.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if u := drain(b); len(u) != 1 || u[0].Status != domain.OrderStatusCancelled {
		t.Fatalf("updates = %+v", u)
	}
	b.OnTick(tick(80, t0.Add(time.Second)))
	if u := drain(b); len(u) != 0 {
		t.Fatalf("cancelled order filled: %+v", u)
	}
	if err := b.Cancel(ctx, id); !errors.Is(err, paper.ErrOrderClosed) {
		t.Errorf("second cancel: err = %v", err)
	}
	if err := b.Cancel(ctx, "paper-99"); !errors.Is(err, paper.ErrUnknownOrder) {
		t.Errorf("unknown: err = %v", err)
	}
}

func TestPlaceRejects(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	ctx := context.Background()
	o := intent("c1", domain.ActionBuy, domain.OrderTypeMarket, 0)
	place(t, b, o)
	if _, err := b.Place(ctx, o); !errors.Is(err, paper.ErrDuplicateTag) {
		t.Errorf("duplicate: err = %v", err)
	}
	unprotected := intent("c2", domain.ActionBuy, domain.OrderTypeMarket, 0)
	unprotected.MarketProtection = 0
	if _, err := b.Place(ctx, unprotected); !errors.Is(err, orders.ErrInvalidIntent) {
		t.Errorf("unprotected market: err = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Place(cancelled, intent("c3", domain.ActionBuy, domain.OrderTypeMarket, 0)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestPlaceRefusesWhenUpdatesCouldBlock(t *testing.T) {
	b, _ := newBroker(t, paper.Config{UpdateBuffer: 3})
	ctx := context.Background()
	place(t, b, intent("c1", domain.ActionBuy, domain.OrderTypeLimit, 90))
	// One ack is queued and c1 can still produce one update: one free slot
	// is not enough for c2's ack and its future update.
	if _, err := b.Place(ctx, intent("c2", domain.ActionBuy, domain.OrderTypeLimit, 90)); !errors.Is(err, paper.ErrUpdateBacklog) {
		t.Fatalf("err = %v, want ErrUpdateBacklog", err)
	}
	drain(b)
	place(t, b, intent("c2", domain.ActionBuy, domain.OrderTypeLimit, 90))
	// Both orders fill, and neither send blocks.
	b.OnTick(tick(80, t0.Add(time.Second)))
	if u := drain(b); len(u) != 3 {
		t.Fatalf("updates = %+v, want ack and two fills", u)
	}
}

func TestOrdersListsLatestState(t *testing.T) {
	b, _ := newBroker(t, paper.Config{})
	ctx := context.Background()
	first := place(t, b, intent("c1", domain.ActionBuy, domain.OrderTypeMarket, 0))
	second := place(t, b, intent("c2", domain.ActionBuy, domain.OrderTypeLimit, 50))
	b.OnTick(tick(100, t0.Add(time.Second)))
	book, err := b.Orders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(book) != 2 || book[0].BrokerID != first || book[0].Status != domain.OrderStatusFilled ||
		book[1].BrokerID != second || book[1].Status != domain.OrderStatusOpen || book[1].ClientID != orders.ClientID("c2") {
		t.Fatalf("book = %+v", book)
	}
}

func TestCharges(t *testing.T) {
	c := paper.ZerodhaIntraday()
	// Turnover 1,00,000: brokerage capped at 20, exchange 2.97, SEBI 0.10,
	// GST 18% of 23.07 = 4.1526; stamp 3 on the buy, STT 25 on the sell.
	if got := c.Charges(domain.ActionBuy, 100, 1000); got != 30.22 {
		t.Errorf("buy charges = %v, want 30.22", got)
	}
	if got := c.Charges(domain.ActionSell, 100, 1000); got != 52.22 {
		t.Errorf("sell charges = %v, want 52.22", got)
	}
	// Turnover 10,000: brokerage 3 is under the cap.
	// 3 + 0.297 + 0.01 + 18% of 3.307 + stamp 0.30 = 4.20226.
	if got := c.Charges(domain.ActionBuy, 10, 1000); got != 4.2 {
		t.Errorf("small buy charges = %v, want 4.20", got)
	}
}
