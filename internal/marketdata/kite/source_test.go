package kite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
)

// fakeTicker records subscriptions and lets a test drive the callbacks.
// serve runs inside ServeWithContext; when it returns, ServeWithContext
// returns too, as the vendor ticker does after giving up.
type fakeTicker struct {
	mu          sync.Mutex
	subscribed  [][]uint32
	modes       []kiteticker.Mode
	accessToken string

	onConnect     func()
	onTick        func(models.Tick)
	onReconnect   func(int, time.Duration)
	onNoReconnect func(int)
	serve         func(ctx context.Context, f *fakeTicker)
}

func (f *fakeTicker) OnConnect(fn func())                     { f.onConnect = fn }
func (f *fakeTicker) OnTick(fn func(models.Tick))             { f.onTick = fn }
func (f *fakeTicker) OnError(func(error))                     {}
func (f *fakeTicker) OnClose(func(int, string))               {}
func (f *fakeTicker) OnReconnect(fn func(int, time.Duration)) { f.onReconnect = fn }
func (f *fakeTicker) OnNoReconnect(fn func(int))              { f.onNoReconnect = fn }
func (f *fakeTicker) ServeWithContext(ctx context.Context)    { f.serve(ctx, f) }
func (f *fakeTicker) Subscribe(tokens []uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed = append(f.subscribed, append([]uint32(nil), tokens...))
	return nil
}
func (f *fakeTicker) SetMode(mode kiteticker.Mode, _ []uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes = append(f.modes, mode)
	return nil
}

var (
	infy  = domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}
	tcs   = domain.Instrument{Token: 2953217, Exchange: "NSE", Symbol: "TCS"}
	recvT = time.Date(2026, 10, 2, 4, 0, 0, 123, time.UTC)
)

func newTestSource(t *testing.T, serve func(context.Context, *fakeTicker)) (*Source, *fakeTicker) {
	t.Helper()
	s, err := New(Config{
		APIKey:      "key",
		Tokens:      staticToken("tok"),
		Instruments: []domain.Instrument{infy, tcs, infy},
		Now:         func() time.Time { return recvT },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.grace = 50 * time.Millisecond
	f := &fakeTicker{serve: serve}
	s.newTicker = func(apiKey, accessToken string) ticker {
		f.accessToken = accessToken
		return f
	}
	return s, f
}

func TestNewValidates(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no key":         {Tokens: staticToken("t"), Instruments: []domain.Instrument{infy}},
		"no tokens":      {APIKey: "k", Instruments: []domain.Instrument{infy}},
		"no instruments": {APIKey: "k", Tokens: staticToken("t")},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRunSubscribesConvertsAndPublishes(t *testing.T) {
	exch := time.Unix(1790919600, 0)
	vendor := models.Tick{
		Mode: "full", InstrumentToken: 408065, LastPrice: 1500.5, LastTradedQuantity: 10,
		AverageTradePrice: 1499.25, VolumeTraded: 123456, TotalBuyQuantity: 1000, TotalSellQuantity: 2000,
		OI: 7, Timestamp: models.Time{Time: exch}, LastTradeTime: models.Time{Time: exch.Add(-time.Second)},
		OHLC: models.OHLC{Open: 1490, High: 1510, Low: 1480, Close: 1495},
	}
	vendor.Depth.Buy[0] = models.DepthItem{Price: 1500.45, Quantity: 50, Orders: 3}
	vendor.Depth.Sell[4] = models.DepthItem{Price: 1501, Quantity: 9, Orders: 1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, f := newTestSource(t, func(ctx context.Context, f *fakeTicker) {
		f.onConnect()
		f.onTick(vendor)
		unsent := vendor
		unsent.Timestamp = models.Time{Time: time.Unix(0, 0)}
		unsent.LastTradeTime = models.Time{Time: time.Unix(0, 0)}
		f.onTick(unsent)
		<-ctx.Done()
	})
	out := make(chan marketdata.Event, 4)
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, out) }()

	ev := <-out
	if ev.Tick == nil || ev.Bar != nil {
		t.Fatalf("event = %+v, want a tick", ev)
	}
	want := domain.Tick{
		InstrumentToken: 408065, Symbol: "NSE:INFY",
		ExchangeTime: exch, ReceivedAt: recvT, LastTradeTime: exch.Add(-time.Second),
		LastPrice: 1500.5, LastQuantity: 10, AveragePrice: 1499.25, Volume: 123456,
		TotalBuyQuantity: 1000, TotalSellQuantity: 2000,
		Open: 1490, High: 1510, Low: 1480, Close: 1495, OI: 7,
	}
	want.Bids[0] = domain.DepthLevel{Price: 1500.45, Quantity: 50, Orders: 3}
	want.Asks[4] = domain.DepthLevel{Price: 1501, Quantity: 9, Orders: 1}
	if !reflect.DeepEqual(*ev.Tick, want) {
		t.Errorf("tick =\n%+v\nwant\n%+v", *ev.Tick, want)
	}
	ev = <-out
	if !ev.Tick.ExchangeTime.IsZero() || !ev.Tick.LastTradeTime.IsZero() {
		t.Errorf("unsent times = %v and %v, want zero", ev.Tick.ExchangeTime, ev.Tick.LastTradeTime)
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("Run = %v, want nil after cancel", err)
	}
	if f.accessToken != "tok" {
		t.Errorf("ticker got access token %q", f.accessToken)
	}
	if want := [][]uint32{{408065, 2953217}}; !reflect.DeepEqual(f.subscribed, want) {
		t.Errorf("subscribed %v, want %v (deduplicated)", f.subscribed, want)
	}
	if want := []kiteticker.Mode{kiteticker.ModeFull}; !reflect.DeepEqual(f.modes, want) {
		t.Errorf("modes %v, want %v", f.modes, want)
	}
	if st := s.Stats(); st.Published != 2 || st.Connects != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRunResubscribesOnReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, f := newTestSource(t, func(ctx context.Context, f *fakeTicker) {
		f.onConnect()
		f.onReconnect(1, 2*time.Second)
		f.onConnect()
		f.onTick(models.Tick{InstrumentToken: 2953217, LastPrice: 4000})
		<-ctx.Done()
	})
	out := make(chan marketdata.Event, 1)
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, out) }()
	if ev := <-out; ev.Tick.Symbol != "NSE:TCS" {
		t.Errorf("symbol = %q", ev.Tick.Symbol)
	}
	cancel()
	<-errc
	if len(f.subscribed) != 2 || len(f.modes) != 2 {
		t.Errorf("subscribed %v with modes %v, want two full subscriptions", f.subscribed, f.modes)
	}
	if st := s.Stats(); st.Connects != 2 || st.Reconnects != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRunDropsWhenConsumerIsSlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan struct{})
	s, _ := newTestSource(t, func(ctx context.Context, f *fakeTicker) {
		f.onConnect()
		for i := range 5 {
			f.onTick(models.Tick{InstrumentToken: 408065, LastPrice: float64(i)})
		}
		close(sent)
		<-ctx.Done()
	})
	out := make(chan marketdata.Event, 2) // nobody reads it
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, out) }()

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick callback blocked on a full channel")
	}
	cancel()
	<-errc
	if st := s.Stats(); st.Published != 2 || st.Dropped != 3 {
		t.Errorf("stats = %+v, want 2 published and 3 dropped", st)
	}
	if got := (<-out).Tick.LastPrice; got != 0 {
		t.Errorf("first buffered tick has price %v, want 0", got)
	}
}

func TestRunReturnsWhenReconnectsAreExhausted(t *testing.T) {
	s, _ := newTestSource(t, func(_ context.Context, f *fakeTicker) {
		f.onReconnect(1, time.Second)
		f.onNoReconnect(2)
	})
	err := s.Run(context.Background(), make(chan marketdata.Event, 1))
	if !errors.Is(err, ErrReconnectExhausted) {
		t.Errorf("Run = %v, want ErrReconnectExhausted", err)
	}
}

func TestRunPublishesNothingAfterReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	finished := make(chan struct{})
	s, _ := newTestSource(t, func(ctx context.Context, f *fakeTicker) {
		defer close(finished)
		<-ctx.Done()
		<-release // a late callback after Run gave up waiting
		f.onTick(models.Tick{InstrumentToken: 408065})
	})
	out := make(chan marketdata.Event, 1)
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, out) }()
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(out) // the caller may close out once Run returns
	close(release)
	<-finished
	if _, ok := <-out; ok {
		t.Error("a tick was published after Run returned")
	}
}

func TestRunTokenError(t *testing.T) {
	s, _ := newTestSource(t, func(context.Context, *fakeTicker) { t.Error("ticker started without a token") })
	s.cfg.Tokens = failingToken{}
	if err := s.Run(context.Background(), make(chan marketdata.Event)); err == nil {
		t.Error("expected a token error")
	}
}
