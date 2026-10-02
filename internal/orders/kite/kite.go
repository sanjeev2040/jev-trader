// Package kite is the live Zerodha Kite broker. It places real orders, so it
// is gated: New refuses to build a broker unless live trading is approved in
// code, live.enabled is true in config and the process runs with --mode=live.
// Live trading is not approved yet; approving it is its own reviewed change.
//
// Orders go over REST with a client-side rate limiter. Order updates arrive
// from the ticker's OnOrderUpdate callback, which the engine routes to
// OnOrderUpdate.
package kite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/orders"
)

// liveApproved is the code-level approval for real-money trading. While it is
// false, New always fails. Setting it to true is the first live enablement and
// must land as a separately reviewed change.
const liveApproved = false

// ModeLive is the --mode value that live trading requires.
const ModeLive = "live"

// Config is the live-trading configuration.
type Config struct {
	LiveEnabled bool   // live.enabled in config.yaml
	Mode        string // the run --mode flag
	Product     string // Kite product; empty means MIS (intraday)
}

// ErrLiveDisabled is returned by New while live trading is not enabled.
var ErrLiveDisabled = errors.New("kite: live trading is disabled")

// API is the part of *kiteconnect.Client the broker uses.
type API interface {
	PlaceOrder(variety string, p kiteconnect.OrderParams) (kiteconnect.OrderResponse, error)
	CancelOrder(variety, orderID string, parentOrderID *string) (kiteconnect.OrderResponse, error)
	GetOrders() (kiteconnect.Orders, error)
}

var _ API = (*kiteconnect.Client)(nil)

// Client-side rate limits, set inside Kite's 10 order requests per second and
// 400 per minute, and 10 requests per second for other endpoints.
const (
	orderBurst     = 10
	ordersPerMin   = 400
	ordersPerSec   = 10
	otherPerSecond = 10
)

// Broker places orders with Kite.
type Broker struct {
	api        API
	product    string
	orderLimit []*rate.Limiter
	otherLimit *rate.Limiter
	updates    *queue
}

var (
	_ orders.Broker    = (*Broker)(nil)
	_ orders.OrderBook = (*Broker)(nil)
)

// New returns a live broker, or ErrLiveDisabled unless live trading is
// approved in code, cfg.LiveEnabled is set and cfg.Mode is ModeLive.
func New(cfg Config, api API) (*Broker, error) {
	if err := gate(liveApproved, cfg); err != nil {
		return nil, err
	}
	if api == nil {
		return nil, errors.New("kite: New needs an API client")
	}
	return newBroker(cfg, api), nil
}

func gate(approved bool, cfg Config) error {
	switch {
	case !approved:
		return fmt.Errorf("%w: not yet approved for real-money trading", ErrLiveDisabled)
	case !cfg.LiveEnabled:
		return fmt.Errorf("%w: live.enabled is not true in config", ErrLiveDisabled)
	case cfg.Mode != ModeLive:
		return fmt.Errorf("%w: --mode is %q, not %q", ErrLiveDisabled, cfg.Mode, ModeLive)
	}
	return nil
}

func newBroker(cfg Config, api API) *Broker {
	product := cfg.Product
	if product == "" {
		product = kiteconnect.ProductMIS
	}
	return &Broker{
		api:     api,
		product: product,
		orderLimit: []*rate.Limiter{
			rate.NewLimiter(ordersPerSec, orderBurst),
			rate.NewLimiter(rate.Every(time.Minute/ordersPerMin), orderBurst),
		},
		otherLimit: rate.NewLimiter(otherPerSecond, otherPerSecond),
		updates:    newQueue(),
	}
}

// Close stops update delivery and closes the Updates channel.
func (b *Broker) Close() { b.updates.close() }

// Updates returns the order-update stream.
func (b *Broker) Updates() <-chan domain.OrderUpdate { return b.updates.out }

// Place places a regular DAY order tagged with the intent's client id. An
// error wraps orders.ErrNotPlaced when the request was never sent or Kite
// rejected it as invalid input.
func (b *Broker) Place(ctx context.Context, intent domain.OrderIntent) (string, error) {
	if err := orders.ValidateIntent(intent); err != nil {
		return "", fmt.Errorf("%w: %w", orders.ErrNotPlaced, err)
	}
	if err := b.waitOrder(ctx); err != nil {
		return "", fmt.Errorf("%w: %w", orders.ErrNotPlaced, err)
	}
	resp, err := b.api.PlaceOrder(kiteconnect.VarietyRegular, b.params(intent))
	var apiErr kiteconnect.Error
	if errors.As(err, &apiErr) && apiErr.ErrorType == kiteconnect.InputError {
		return "", fmt.Errorf("%w: kite: place %s: %w", orders.ErrNotPlaced, orders.ClientID(intent.CycleID), err)
	}
	if err != nil {
		return "", fmt.Errorf("kite: place %s: %w", orders.ClientID(intent.CycleID), err)
	}
	return resp.OrderID, nil
}

func (b *Broker) params(o domain.OrderIntent) kiteconnect.OrderParams {
	spec := orders.SpecFor(o)
	p := kiteconnect.OrderParams{
		Exchange:        o.Instrument.Exchange,
		Tradingsymbol:   o.Instrument.Symbol,
		Validity:        kiteconnect.ValidityDay,
		Product:         b.product,
		OrderType:       string(spec.Type),
		TransactionType: string(o.Side),
		Quantity:        int(o.Quantity),
		Tag:             orders.ClientID(o.CycleID),
	}
	if spec.Type == domain.OrderTypeLimit {
		p.Price = spec.LimitPrice
	} else {
		p.MarketProtection = spec.MarketProtection
	}
	return p
}

// Cancel cancels a regular order.
func (b *Broker) Cancel(ctx context.Context, brokerID string) error {
	if err := b.waitOrder(ctx); err != nil {
		return err
	}
	if _, err := b.api.CancelOrder(kiteconnect.VarietyRegular, brokerID, nil); err != nil {
		return fmt.Errorf("kite: cancel %s: %w", brokerID, err)
	}
	return nil
}

// Orders returns the latest state of every order on the account today.
func (b *Broker) Orders(ctx context.Context) ([]domain.OrderUpdate, error) {
	if err := b.otherLimit.Wait(ctx); err != nil {
		return nil, err
	}
	book, err := b.api.GetOrders()
	if err != nil {
		return nil, fmt.Errorf("kite: list orders: %w", err)
	}
	out := make([]domain.OrderUpdate, len(book))
	for i, o := range book {
		out[i] = toUpdate(o)
	}
	return out, nil
}

// OnOrderUpdate converts a ticker order update and queues it on Updates. It
// never blocks, so it is safe to call from the ticker's callback.
func (b *Broker) OnOrderUpdate(o kiteconnect.Order) { b.updates.push(toUpdate(o)) }

func (b *Broker) waitOrder(ctx context.Context) error {
	for _, l := range b.orderLimit {
		if err := l.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

func toUpdate(o kiteconnect.Order) domain.OrderUpdate {
	at := o.ExchangeUpdateTimestamp.Time
	if at.IsZero() {
		at = o.OrderTimestamp.Time
	}
	return domain.OrderUpdate{
		ClientID:       tagOf(o),
		BrokerID:       o.OrderID,
		Status:         statusOf(o.Status),
		FilledQuantity: int64(o.FilledQuantity),
		AveragePrice:   o.AveragePrice,
		Reason:         o.StatusMessage,
		Time:           at,
	}
}

// tagOf returns the order's client id: the first tag with this system's
// prefix, else the primary tag.
func tagOf(o kiteconnect.Order) string {
	for _, t := range append([]string{o.Tag}, o.Tags...) {
		if strings.HasPrefix(t, orders.ClientIDPrefix) {
			return t
		}
	}
	return o.Tag
}

// statusOf maps Kite's order status. Every non-terminal Kite status (OPEN,
// OPEN PENDING, TRIGGER PENDING, UPDATE, ...) is an open order.
func statusOf(s string) domain.OrderStatus {
	switch {
	case s == "COMPLETE":
		return domain.OrderStatusFilled
	case s == "REJECTED":
		return domain.OrderStatusRejected
	case strings.HasPrefix(s, "CANCELLED"):
		return domain.OrderStatusCancelled
	}
	return domain.OrderStatusOpen
}

// queue delivers updates in order without ever blocking the producer.
type queue struct {
	mu      sync.Mutex
	items   []domain.OrderUpdate
	wake    chan struct{}
	done    chan struct{}
	stopped sync.Once
	out     chan domain.OrderUpdate
}

func newQueue() *queue {
	q := &queue{
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
		out:  make(chan domain.OrderUpdate),
	}
	go q.pump()
	return q
}

func (q *queue) push(u domain.OrderUpdate) {
	q.mu.Lock()
	q.items = append(q.items, u)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) close() { q.stopped.Do(func() { close(q.done) }) }

func (q *queue) pump() {
	defer close(q.out)
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			select {
			case <-q.wake:
				continue
			case <-q.done:
				return
			}
		}
		u := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.out <- u:
		case <-q.done:
			return
		}
	}
}
