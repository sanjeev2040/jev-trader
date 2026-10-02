package domain

import "time"

// OrderType is the broker order type.
type OrderType string

// Order types. The order manager derives the type from the approved
// intent (see orders.SpecFor). Market orders must carry non-zero market
// protection.
const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
)

// OrderStatus is the broker-side state of an order.
type OrderStatus string

// Order statuses. Filled, Cancelled and Rejected are terminal.
const (
	OrderStatusOpen      OrderStatus = "OPEN"      // acknowledged and working, possibly partly filled
	OrderStatusFilled    OrderStatus = "FILLED"    // fully filled
	OrderStatusCancelled OrderStatus = "CANCELLED" // cancelled, possibly after a partial fill
	OrderStatusRejected  OrderStatus = "REJECTED"  // rejected by the broker or exchange
)

// OrderUpdate is one broker event for an order: an acknowledgement, a
// rejection, a cancellation, or a partial or full fill. Quantities are
// cumulative, so a repeated or late update carries no new fill.
type OrderUpdate struct {
	ClientID       string // the order tag
	BrokerID       string
	Status         OrderStatus
	FilledQuantity int64   // cumulative filled quantity
	AveragePrice   float64 // average price of FilledQuantity
	// Costs is the cumulative charges for FilledQuantity when the broker
	// reports them (the paper broker does; Kite order updates do not).
	Costs  float64
	Reason string // rejection or cancellation message
	Time   time.Time
}
