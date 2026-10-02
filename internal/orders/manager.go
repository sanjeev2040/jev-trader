package orders

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// Status is the manager's view of an order.
type Status string

// Order statuses. Filled, Cancelled, Rejected and NotPlaced are terminal, but
// a broker update for a NotPlaced order still moves it to the reported status.
const (
	// StatusPending: the intent is recorded and placed, and the broker has not
	// acknowledged it yet.
	StatusPending Status = "PENDING"
	// StatusPlaceFailed: Place returned an error other than ErrNotPlaced. The
	// broker may still hold the order (a timeout, say), so a later update or
	// Reconcile can revive it.
	StatusPlaceFailed Status = "PLACE_FAILED"
	StatusOpen        Status = "OPEN"
	StatusFilled      Status = "FILLED"
	StatusCancelled   Status = "CANCELLED"
	StatusRejected    Status = "REJECTED"
	// StatusNotPlaced: Reconcile found a recorded intent the broker did not
	// list. It is not placed again; its decision is stale. If the broker
	// lists it late, its updates still apply.
	StatusNotPlaced Status = "NOT_PLACED"
)

// Terminal reports whether the order can no longer change.
func (s Status) Terminal() bool {
	switch s {
	case StatusFilled, StatusCancelled, StatusRejected, StatusNotPlaced:
		return true
	}
	return false
}

// rank orders statuses so that a late or repeated update never moves an order
// backwards.
func (s Status) rank() int {
	switch {
	case s == StatusNotPlaced:
		return 0
	case s.Terminal():
		return 2
	case s == StatusOpen:
		return 1
	}
	return 0
}

func statusOf(s domain.OrderStatus) (Status, bool) {
	switch s {
	case domain.OrderStatusOpen:
		return StatusOpen, true
	case domain.OrderStatusFilled:
		return StatusFilled, true
	case domain.OrderStatusCancelled:
		return StatusCancelled, true
	case domain.OrderStatusRejected:
		return StatusRejected, true
	}
	return "", false
}

// Order is the tracked state of one placed intent.
type Order struct {
	Intent         domain.OrderIntent
	BrokerID       string
	Status         Status
	FilledQuantity int64
	AveragePrice   float64
	Costs          float64
	Reason         string // rejection, cancellation or placement error
	UpdatedAt      time.Time
}

// Journal durably records intents. The manager records each intent before it
// places it, so a crash after placement can always be reconciled. The engine
// backs it with the decision log.
type Journal interface {
	RecordIntent(ctx context.Context, intent domain.OrderIntent) error
}

// Errors returned by the manager.
var (
	ErrNotReconciled    = errors.New("orders: Reconcile must run before the first Submit")
	ErrZeroApproval     = errors.New("orders: zero risk.Approval authorizes nothing")
	ErrAlreadySubmitted = errors.New("orders: cycle already submitted")
	ErrUnknownOrder     = errors.New("orders: update for an unknown order")
	ErrBrokerIDMismatch = errors.New("orders: update broker id does not match the order")
	ErrOverfill         = errors.New("orders: filled quantity exceeds the order quantity")
	ErrOrderClosed      = errors.New("orders: order is already terminal")
	ErrNotAcknowledged  = errors.New("orders: order has no broker id yet")
	ErrNoOrderBook      = errors.New("orders: broker cannot list its orders")
)

// Manager submits approved intents to a broker and tracks them to completion.
type Manager struct {
	broker     Broker
	journal    Journal
	orders     map[string]*Order // by client id
	byBroker   map[string]string // broker id → client id
	reconciled bool
}

// NewManager returns a manager that places orders through broker and records
// intents in journal.
func NewManager(broker Broker, journal Journal) (*Manager, error) {
	if broker == nil || journal == nil {
		return nil, errors.New("orders: NewManager needs a broker and a journal")
	}
	return &Manager{
		broker:   broker,
		journal:  journal,
		orders:   make(map[string]*Order),
		byBroker: make(map[string]string),
	}, nil
}

// Submit places the approved intent. It is idempotent per decision cycle: a
// cycle that was already submitted is never placed again, and Submit returns
// its existing order with ErrAlreadySubmitted.
//
// The intent is recorded in the journal before it is placed. If recording
// fails, nothing is placed. If Place fails, the error is returned and the
// order is kept as StatusRejected when the error wraps ErrNotPlaced, else as
// StatusPlaceFailed.
func (m *Manager) Submit(ctx context.Context, a risk.Approval) (Order, error) {
	if !m.reconciled {
		return Order{}, ErrNotReconciled
	}
	if !a.Valid() {
		return Order{}, ErrZeroApproval
	}
	intent := a.Intent()
	if err := ValidateIntent(intent); err != nil {
		return Order{}, err
	}
	clientID := ClientID(intent.CycleID)
	if o, ok := m.orders[clientID]; ok {
		return *o, ErrAlreadySubmitted
	}
	if err := m.journal.RecordIntent(ctx, intent); err != nil {
		return Order{}, fmt.Errorf("orders: record intent %s: %w", clientID, err)
	}
	o := &Order{Intent: intent, Status: StatusPending}
	m.orders[clientID] = o
	brokerID, err := m.broker.Place(ctx, intent)
	if err != nil {
		o.Status = StatusPlaceFailed
		if errors.Is(err, ErrNotPlaced) {
			o.Status = StatusRejected
		}
		o.Reason = err.Error()
		return *o, fmt.Errorf("orders: place %s: %w", clientID, err)
	}
	o.BrokerID = brokerID
	m.byBroker[brokerID] = clientID
	return *o, nil
}

// Apply folds one broker update into its order. It returns the updated order
// and, if the update reports newly filled quantity, the new fill. Updates may
// repeat or arrive out of order: filled quantity only grows and status never
// moves backwards. ErrOverfill is returned alongside the order and fill when
// the broker reports more than the order quantity.
func (m *Manager) Apply(u domain.OrderUpdate) (Order, *domain.Fill, error) {
	clientID := u.ClientID
	if _, ok := m.orders[clientID]; !ok {
		clientID = m.byBroker[u.BrokerID]
	}
	o, ok := m.orders[clientID]
	if !ok {
		return Order{}, nil, fmt.Errorf("%w: tag %q, broker id %q", ErrUnknownOrder, u.ClientID, u.BrokerID)
	}
	if u.BrokerID != "" {
		switch o.BrokerID {
		case "":
			o.BrokerID = u.BrokerID
			m.byBroker[u.BrokerID] = clientID
		case u.BrokerID:
		default:
			return *o, nil, fmt.Errorf("%w: %s has %q, update has %q", ErrBrokerIDMismatch, clientID, o.BrokerID, u.BrokerID)
		}
	}

	var fill *domain.Fill
	if u.FilledQuantity > o.FilledQuantity {
		qty := u.FilledQuantity - o.FilledQuantity
		notional := u.AveragePrice*float64(u.FilledQuantity) - o.AveragePrice*float64(o.FilledQuantity)
		fill = &domain.Fill{
			ClientID:   clientID,
			Instrument: o.Intent.Instrument,
			Side:       o.Intent.Side,
			Quantity:   qty,
			Price:      notional / float64(qty),
			Charges:    max(u.Costs-o.Costs, 0),
			Time:       u.Time,
		}
		o.FilledQuantity = u.FilledQuantity
		o.AveragePrice = u.AveragePrice
		o.Costs = max(u.Costs, o.Costs)
	}
	if s, ok := statusOf(u.Status); ok && (o.Status == StatusNotPlaced || !o.Status.Terminal()) && s.rank() >= o.Status.rank() {
		o.Status = s
		if u.Reason != "" {
			o.Reason = u.Reason
		}
	}
	if u.Time.After(o.UpdatedAt) {
		o.UpdatedAt = u.Time
	}
	if o.FilledQuantity > o.Intent.Quantity {
		return *o, fill, fmt.Errorf("%w: %s filled %d of %d", ErrOverfill, clientID, o.FilledQuantity, o.Intent.Quantity)
	}
	return *o, fill, nil
}

// Cancel asks the broker to cancel an open order. The resulting status
// arrives as an update.
func (m *Manager) Cancel(ctx context.Context, clientID string) error {
	o, ok := m.orders[clientID]
	switch {
	case !ok:
		return fmt.Errorf("%w: %s", ErrUnknownOrder, clientID)
	case o.Status.Terminal():
		return fmt.Errorf("%w: %s is %s", ErrOrderClosed, clientID, o.Status)
	case o.BrokerID == "":
		return fmt.Errorf("%w: %s", ErrNotAcknowledged, clientID)
	}
	return m.broker.Cancel(ctx, o.BrokerID)
}

// Order returns the order for a client id.
func (m *Manager) Order(clientID string) (Order, bool) {
	o, ok := m.orders[clientID]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Open returns every non-terminal order, sorted by client id.
func (m *Manager) Open() []Order {
	var open []Order
	for _, o := range m.orders {
		if !o.Status.Terminal() {
			open = append(open, *o)
		}
	}
	slices.SortFunc(open, func(a, b Order) int { return strings.Compare(ClientID(a.Intent.CycleID), ClientID(b.Intent.CycleID)) })
	return open
}

// Reconciliation is what Reconcile found.
type Reconciliation struct {
	// Adopted are recorded intents the broker holds, now tracked with the
	// broker's latest state.
	Adopted []Order
	// NotPlaced are recorded intents the broker did not list. They are
	// marked StatusNotPlaced and not placed again.
	NotPlaced []Order
	// Unrecorded are broker orders carrying this system's tag prefix that no
	// recorded intent explains. They need an operator's attention.
	Unrecorded []domain.OrderUpdate
}

// Reconcile matches the intents recorded before a restart against the
// broker's order book by tag. It must run once before the first Submit, with
// no intents on a fresh start.
func (m *Manager) Reconcile(ctx context.Context, intents []domain.OrderIntent) (Reconciliation, error) {
	book, ok := m.broker.(OrderBook)
	if !ok {
		return Reconciliation{}, ErrNoOrderBook
	}
	held, err := book.Orders(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("orders: list broker orders: %w", err)
	}
	byTag := make(map[string]domain.OrderUpdate, len(held))
	for _, u := range held {
		if u.ClientID != "" {
			byTag[u.ClientID] = u
		}
	}

	var r Reconciliation
	recorded := make(map[string]bool, len(intents))
	for _, intent := range intents {
		if intent.CycleID == "" {
			return r, fmt.Errorf("%w: recorded intent has no cycle id", ErrInvalidIntent)
		}
		clientID := ClientID(intent.CycleID)
		recorded[clientID] = true
		if _, known := m.orders[clientID]; known {
			continue
		}
		o := &Order{Intent: intent, Status: StatusPending}
		m.orders[clientID] = o
		u, ok := byTag[clientID]
		if !ok {
			o.Status = StatusNotPlaced
			o.Reason = "recorded but never reached the broker"
			r.NotPlaced = append(r.NotPlaced, *o)
			continue
		}
		adopted, _, err := m.Apply(u)
		if err != nil {
			return r, fmt.Errorf("orders: adopt %s: %w", clientID, err)
		}
		r.Adopted = append(r.Adopted, adopted)
	}
	for _, u := range held {
		if strings.HasPrefix(u.ClientID, ClientIDPrefix) && !recorded[u.ClientID] {
			if _, known := m.orders[u.ClientID]; !known {
				r.Unrecorded = append(r.Unrecorded, u)
			}
		}
	}
	m.reconciled = true
	return r, nil
}
