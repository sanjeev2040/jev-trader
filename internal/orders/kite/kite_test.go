package kite

import (
	"context"
	"errors"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
)

// fakeAPI records calls. It never touches the network.
type fakeAPI struct {
	variety  []string
	placed   []kiteconnect.OrderParams
	canceled []string
	book     kiteconnect.Orders
	err      error
}

func (f *fakeAPI) PlaceOrder(variety string, p kiteconnect.OrderParams) (kiteconnect.OrderResponse, error) {
	f.variety = append(f.variety, variety)
	f.placed = append(f.placed, p)
	return kiteconnect.OrderResponse{OrderID: "250930000000001"}, f.err
}

func (f *fakeAPI) CancelOrder(variety, orderID string, _ *string) (kiteconnect.OrderResponse, error) {
	f.variety = append(f.variety, variety)
	f.canceled = append(f.canceled, orderID)
	return kiteconnect.OrderResponse{OrderID: orderID}, f.err
}

func (f *fakeAPI) GetOrders() (kiteconnect.Orders, error) { return f.book, f.err }

func TestLiveTradingIsNotApproved(t *testing.T) {
	// This test fails on purpose when live trading is approved, so that
	// change is visible in review.
	if liveApproved {
		t.Fatal("liveApproved is true; enabling live trading needs its own reviewed change")
	}
	for _, cfg := range []Config{
		{},
		{LiveEnabled: true},
		{Mode: ModeLive},
		{LiveEnabled: true, Mode: ModeLive},
	} {
		b, err := New(cfg, &fakeAPI{})
		if b != nil || !errors.Is(err, ErrLiveDisabled) {
			t.Errorf("New(%+v) = %v, %v; want ErrLiveDisabled", cfg, b, err)
		}
	}
}

func TestGate(t *testing.T) {
	cases := []struct {
		approved bool
		cfg      Config
		ok       bool
	}{
		{false, Config{LiveEnabled: true, Mode: ModeLive}, false},
		{true, Config{LiveEnabled: false, Mode: ModeLive}, false},
		{true, Config{LiveEnabled: true, Mode: "paper"}, false},
		{true, Config{LiveEnabled: true, Mode: ""}, false},
		{true, Config{LiveEnabled: true, Mode: ModeLive}, true},
	}
	for _, c := range cases {
		err := gate(c.approved, c.cfg)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrLiveDisabled)) {
			t.Errorf("gate(%v, %+v) = %v", c.approved, c.cfg, err)
		}
	}
}

func intent(cycle string, typ domain.OrderType) domain.OrderIntent {
	o := domain.OrderIntent{
		CycleID:    cycle,
		Instrument: domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"},
		Side:       domain.ActionSell,
		Quantity:   5,
	}
	if typ == domain.OrderTypeLimit {
		o.RefPrice = 1500.5
	}
	return o
}

func TestPlaceParams(t *testing.T) {
	api := &fakeAPI{}
	b := newBroker(Config{}, api)
	defer b.Close()
	ctx := context.Background()

	id, err := b.Place(ctx, intent("c1", domain.OrderTypeLimit))
	if err != nil || id != "250930000000001" {
		t.Fatalf("Place = %q, %v", id, err)
	}
	if _, err := b.Place(ctx, intent("c2", domain.OrderTypeMarket)); err != nil {
		t.Fatal(err)
	}
	limit, market := api.placed[0], api.placed[1]
	want := kiteconnect.OrderParams{
		Exchange: "NSE", Tradingsymbol: "INFY", Validity: "DAY", Product: "MIS",
		OrderType: "LIMIT", TransactionType: "SELL", Quantity: 5, Price: 1500.5,
		Tag: orders.ClientID("c1"),
	}
	if limit != want {
		t.Errorf("limit params = %+v, want %+v", limit, want)
	}
	if market.OrderType != "MARKET" || market.MarketProtection != 2 || market.Price != 0 || market.Tag != orders.ClientID("c2") {
		t.Errorf("market params = %+v", market)
	}
	for _, v := range api.variety {
		if v != kiteconnect.VarietyRegular {
			t.Errorf("variety %q, want regular", v)
		}
	}
}

func TestPlaceRejectsInvalidIntentLocally(t *testing.T) {
	api := &fakeAPI{}
	b := newBroker(Config{}, api)
	defer b.Close()
	bad := intent("c1", domain.OrderTypeMarket)
	bad.Quantity = 0
	if _, err := b.Place(context.Background(), bad); !errors.Is(err, orders.ErrInvalidIntent) || !errors.Is(err, orders.ErrNotPlaced) {
		t.Fatalf("err = %v, want ErrInvalidIntent and ErrNotPlaced", err)
	}
	if len(api.placed) != 0 {
		t.Fatal("invalid intent reached the API")
	}
}

func TestPlaceAndCancelErrors(t *testing.T) {
	api := &fakeAPI{err: errors.New("IP not whitelisted")}
	b := newBroker(Config{}, api)
	defer b.Close()
	ctx := context.Background()
	if _, err := b.Place(ctx, intent("c1", domain.OrderTypeLimit)); err == nil || errors.Is(err, orders.ErrNotPlaced) {
		t.Errorf("unknown API error: err = %v, want an error that may have placed", err)
	}
	if err := b.Cancel(ctx, "1"); err == nil {
		t.Error("Cancel swallowed the API error")
	}
	if _, err := b.Orders(ctx); err == nil {
		t.Error("Orders swallowed the API error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Place(cancelled, intent("c2", domain.OrderTypeLimit)); !errors.Is(err, context.Canceled) || !errors.Is(err, orders.ErrNotPlaced) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestPlaceClassifiesKiteErrors(t *testing.T) {
	cases := []struct {
		err       error
		notPlaced bool
	}{
		{kiteconnect.NewError(kiteconnect.InputError, "Invalid tradingsymbol", nil), true},
		{kiteconnect.NewError(kiteconnect.NetworkError, "Request failed.", nil), false},
		{kiteconnect.NewError(kiteconnect.GeneralError, "Something went wrong", nil), false},
	}
	for _, c := range cases {
		b := newBroker(Config{}, &fakeAPI{err: c.err})
		_, err := b.Place(context.Background(), intent("c1", domain.OrderTypeLimit))
		b.Close()
		if !errors.Is(err, c.err) || errors.Is(err, orders.ErrNotPlaced) != c.notPlaced {
			t.Errorf("%v: err = %v, want ErrNotPlaced %v", c.err, err, c.notPlaced)
		}
	}
}

func TestCancelAndOrders(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 1, 0, time.UTC)
	api := &fakeAPI{book: kiteconnect.Orders{
		{OrderID: "1", Tag: orders.ClientID("c1"), Status: "OPEN", FilledQuantity: 2, AveragePrice: 1500, ExchangeUpdateTimestamp: models.Time{Time: at}},
		{OrderID: "2", Tag: "manual", Status: "COMPLETE"},
	}}
	b := newBroker(Config{Product: kiteconnect.ProductCNC}, api)
	defer b.Close()
	ctx := context.Background()
	if err := b.Cancel(ctx, "1"); err != nil || len(api.canceled) != 1 || api.canceled[0] != "1" {
		t.Fatalf("Cancel: %v, canceled %v", err, api.canceled)
	}
	book, err := b.Orders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.OrderUpdate{ClientID: orders.ClientID("c1"), BrokerID: "1", Status: domain.OrderStatusOpen, FilledQuantity: 2, AveragePrice: 1500, Time: at}
	if len(book) != 2 || book[0] != want || book[1].Status != domain.OrderStatusFilled {
		t.Fatalf("book = %+v", book)
	}
	if b.product != kiteconnect.ProductCNC {
		t.Errorf("product = %q, want configured CNC", b.product)
	}
}

func TestToUpdate(t *testing.T) {
	placed := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	tag := orders.ClientID("c1")
	cases := []struct {
		status string
		want   domain.OrderStatus
	}{
		{"OPEN PENDING", domain.OrderStatusOpen},
		{"OPEN", domain.OrderStatusOpen},
		{"UPDATE", domain.OrderStatusOpen},
		{"TRIGGER PENDING", domain.OrderStatusOpen},
		{"COMPLETE", domain.OrderStatusFilled},
		{"REJECTED", domain.OrderStatusRejected},
		{"CANCELLED", domain.OrderStatusCancelled},
		{"CANCELLED AMO", domain.OrderStatusCancelled},
	}
	for _, c := range cases {
		u := toUpdate(kiteconnect.Order{
			OrderID: "9", Status: c.status, StatusMessage: "msg",
			Tags: []string{"other", tag}, FilledQuantity: 3, AveragePrice: 10.5,
			OrderTimestamp: models.Time{Time: placed},
		})
		want := domain.OrderUpdate{ClientID: tag, BrokerID: "9", Status: c.want, FilledQuantity: 3, AveragePrice: 10.5, Reason: "msg", Time: placed}
		if u != want {
			t.Errorf("%s: %+v, want %+v", c.status, u, want)
		}
	}
}

func TestOnOrderUpdateNeverBlocks(t *testing.T) {
	b := newBroker(Config{}, &fakeAPI{})
	const n = 1000
	for i := range n {
		b.OnOrderUpdate(kiteconnect.Order{OrderID: "1", Status: "OPEN", FilledQuantity: float64(i)})
	}
	for i := range n {
		select {
		case u := <-b.Updates():
			if u.FilledQuantity != int64(i) {
				t.Fatalf("update %d out of order: %+v", i, u)
			}
		case <-time.After(time.Second):
			t.Fatalf("update %d not delivered", i)
		}
	}
	b.Close()
	b.Close() // idempotent
	select {
	case _, ok := <-b.Updates():
		if ok {
			t.Fatal("update after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Updates not closed")
	}
}
