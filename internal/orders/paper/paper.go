// Package paper is the simulated broker used for paper trading and backtests.
// It fills orders against the market-data stream at the first event after
// order time plus the configured latency, with configurable slippage and
// costs, and emits the same order updates as the Kite broker.
//
// Updates are sent synchronously by the call that causes them (Place, Cancel,
// OnTick, OnBar) and never block: Place refuses a new order unless the update
// buffer has room for every update the open orders can still produce. A
// caller that drains Updates after each call therefore sees a deterministic
// event order, which replay relies on.
package paper

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
)

// Clock is the part of clock.Clock the broker needs.
type Clock interface {
	Now() time.Time
}

// Config tunes the simulation.
type Config struct {
	// SlippageBps moves each fill price against the order, in basis points
	// of the reference price: buys pay more and sells receive less. A limit
	// order never fills beyond its limit.
	SlippageBps float64
	// TickSize is the price grid fills are rounded to, against the order.
	// Zero means DefaultTickSize.
	TickSize float64
	// Costs prices each fill.
	Costs CostModel
	// UpdateBuffer is the capacity of the Updates channel. Zero means
	// DefaultUpdateBuffer.
	UpdateBuffer int
	// Latency delays when an order can fill: an order placed at the clock's
	// Now fills only on a tick after Now+Latency or a bar that starts at or
	// after it. Zero means the first event after Now.
	Latency time.Duration
}

// Defaults.
const (
	DefaultTickSize     = 0.05 // NSE equity tick size, in rupees
	DefaultUpdateBuffer = 1024
)

// DefaultConfig is paper trading with Zerodha intraday charges and 2 bps of
// slippage.
func DefaultConfig() Config {
	return Config{SlippageBps: 2, Costs: ZerodhaIntraday()}
}

// Errors returned by the broker.
var (
	ErrDuplicateTag  = errors.New("paper: an order with this tag already exists")
	ErrUnknownOrder  = errors.New("paper: unknown broker id")
	ErrOrderClosed   = errors.New("paper: order is already terminal")
	ErrUpdateBacklog = errors.New("paper: update buffer is full; drain Updates")
)

type order struct {
	intent    domain.OrderIntent
	spec      orders.Spec
	clientID  string
	brokerID  string
	fillsFrom time.Time // order time plus latency
	last      domain.OrderUpdate
}

// Broker is the paper broker. It is safe for concurrent use.
type Broker struct {
	clock Clock
	cfg   Config

	mu      sync.Mutex
	seq     int
	orders  map[string]*order // by broker id
	byTag   map[string]string // tag → broker id
	all     []*order          // in placement order
	open    []*order          // in placement order
	updates chan domain.OrderUpdate
}

var (
	_ orders.Broker    = (*Broker)(nil)
	_ orders.OrderBook = (*Broker)(nil)
)

// New returns a paper broker that reads order time from clock.
func New(clock Clock, cfg Config) (*Broker, error) {
	if clock == nil {
		return nil, errors.New("paper: New needs a clock")
	}
	if cfg.SlippageBps < 0 || cfg.TickSize < 0 || cfg.UpdateBuffer < 0 || cfg.Latency < 0 {
		return nil, fmt.Errorf("paper: negative config value: %+v", cfg)
	}
	if cfg.TickSize == 0 {
		cfg.TickSize = DefaultTickSize
	}
	if cfg.UpdateBuffer == 0 {
		cfg.UpdateBuffer = DefaultUpdateBuffer
	}
	return &Broker{
		clock:   clock,
		cfg:     cfg,
		orders:  make(map[string]*order),
		byTag:   make(map[string]string),
		updates: make(chan domain.OrderUpdate, cfg.UpdateBuffer),
	}, nil
}

// Updates returns the order-update stream. It is never closed.
func (b *Broker) Updates() <-chan domain.OrderUpdate { return b.updates }

// Place accepts an order and acknowledges it with an OPEN update. It fills on
// a later OnTick or OnBar. Every error wraps orders.ErrNotPlaced.
func (b *Broker) Place(ctx context.Context, intent domain.OrderIntent) (string, error) {
	brokerID, err := b.place(ctx, intent)
	if err != nil {
		return "", fmt.Errorf("%w: %w", orders.ErrNotPlaced, err)
	}
	return brokerID, nil
}

func (b *Broker) place(ctx context.Context, intent domain.OrderIntent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := orders.ValidateIntent(intent); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.byTag[orders.ClientID(intent.CycleID)]; ok {
		return "", fmt.Errorf("%w: %s", ErrDuplicateTag, orders.ClientID(intent.CycleID))
	}
	// The ack takes one slot now, and every open order, this one included,
	// can still produce one terminal update.
	if free := cap(b.updates) - len(b.updates); free < len(b.open)+2 {
		return "", ErrUpdateBacklog
	}
	b.seq++
	now := b.clock.Now()
	o := &order{
		intent:    intent,
		spec:      orders.SpecFor(intent),
		clientID:  orders.ClientID(intent.CycleID),
		brokerID:  fmt.Sprintf("paper-%d", b.seq),
		fillsFrom: now.Add(b.cfg.Latency),
	}
	b.orders[o.brokerID] = o
	b.byTag[o.clientID] = o.brokerID
	b.all = append(b.all, o)
	b.open = append(b.open, o)
	b.emit(o, domain.OrderUpdate{Status: domain.OrderStatusOpen, Time: now})
	return o.brokerID, nil
}

// Cancel cancels an open order and emits a CANCELLED update.
func (b *Broker) Cancel(ctx context.Context, brokerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.orders[brokerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownOrder, brokerID)
	}
	if o.last.Status != domain.OrderStatusOpen {
		return fmt.Errorf("%w: %s is %s", ErrOrderClosed, brokerID, o.last.Status)
	}
	b.close(o)
	b.emit(o, domain.OrderUpdate{Status: domain.OrderStatusCancelled, Reason: "cancelled by client", Time: b.clock.Now()})
	return nil
}

// Orders returns every order's latest update, in placement order.
func (b *Broker) Orders(ctx context.Context) ([]domain.OrderUpdate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]domain.OrderUpdate, 0, len(b.all))
	for _, o := range b.all {
		out = append(out, o.last)
	}
	return out, nil
}

// OnTick offers a tick to the open orders on its instrument. An order fills
// at the tick's last price when the tick's exchange time (or receive time, if
// that is unset) is after the order time plus latency.
func (b *Broker) OnTick(t domain.Tick) {
	at := t.ExchangeTime
	if at.IsZero() {
		at = t.ReceivedAt
	}
	b.offer(t.InstrumentToken, at, func(fillsFrom time.Time) bool { return at.After(fillsFrom) },
		t.LastPrice, t.LastPrice, t.LastPrice)
}

// OnBar offers a closed bar to the open orders on its instrument. Only a bar
// that starts at or after the order time plus latency is used, so no price
// from before the order can fill it. A market order fills at the bar's open; a limit order
// fills at the open if that is within the limit, else at the limit if the
// bar's range reached it.
func (b *Broker) OnBar(bar domain.Bar) {
	b.offer(bar.Instrument.Token, bar.End, func(fillsFrom time.Time) bool { return !bar.Start.Before(fillsFrom) },
		bar.Open, bar.Low, bar.High)
}

func (b *Broker) offer(token uint32, at time.Time, after func(fillsFrom time.Time) bool, first, low, high float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, o := range slices.Clone(b.open) {
		if o.intent.Instrument.Token != token || !after(o.fillsFrom) {
			continue
		}
		price, ok := b.fillPrice(o, first, low, high)
		if !ok {
			continue
		}
		qty := o.intent.Quantity
		b.close(o)
		b.emit(o, domain.OrderUpdate{
			Status:         domain.OrderStatusFilled,
			FilledQuantity: qty,
			AveragePrice:   price,
			Costs:          b.cfg.Costs.Charges(o.intent.Side, qty, price),
			Time:           at,
		})
	}
}

// fillPrice returns the price an order fills at, given the first price after
// order time and the range that follows it, or false if it does not fill.
func (b *Broker) fillPrice(o *order, first, low, high float64) (float64, bool) {
	if !(first > 0) {
		return 0, false
	}
	slip := b.cfg.SlippageBps / 10_000
	if o.intent.Side == domain.ActionBuy {
		px := roundUp(first*(1+slip), b.cfg.TickSize)
		switch {
		case o.spec.Type == domain.OrderTypeMarket:
			return px, true
		case first <= o.spec.LimitPrice:
			return min(px, o.spec.LimitPrice), true
		case low <= o.spec.LimitPrice:
			return o.spec.LimitPrice, true
		}
		return 0, false
	}
	px := roundDown(first*(1-slip), b.cfg.TickSize)
	switch {
	case o.spec.Type == domain.OrderTypeMarket:
		return px, true
	case first >= o.spec.LimitPrice:
		return max(px, o.spec.LimitPrice), true
	case high >= o.spec.LimitPrice:
		return o.spec.LimitPrice, true
	}
	return 0, false
}

// emit records u as o's latest state and sends it. Place's buffer check
// guarantees the send does not block.
func (b *Broker) emit(o *order, u domain.OrderUpdate) {
	u.ClientID = o.clientID
	u.BrokerID = o.brokerID
	o.last = u
	b.updates <- u
}

func (b *Broker) close(o *order) {
	b.open = slices.DeleteFunc(b.open, func(x *order) bool { return x == o })
}

// roundUp and roundDown move a price onto the tick grid, against the order.
// The epsilon keeps a price already on the grid from moving a whole tick
// because of float error.
func roundUp(p, tick float64) float64 {
	return roundPaise(math.Ceil(p/tick-1e-9) * tick)
}

func roundDown(p, tick float64) float64 {
	return roundPaise(math.Floor(p/tick+1e-9) * tick)
}
