// Package orders is the order manager (issue #9). It places only orders that
// carry a risk.Approval, places each decision cycle's order at most once, and
// tracks fills from broker order updates.
//
// The manager is owned by the engine loop and is not safe for concurrent use.
// Brokers live in orders/paper (simulated fills, the default) and orders/kite
// (real orders, gated off until live trading is explicitly approved).
// Order modification (Kite's 25-modification cap with cancel-and-replace) is
// deferred to issue #17.
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Broker places and cancels orders and reports their progress.
type Broker interface {
	Place(ctx context.Context, o domain.OrderIntent) (brokerID string, err error)
	Cancel(ctx context.Context, brokerID string) error
	Updates() <-chan domain.OrderUpdate // acks, rejections, partial and full fills
}

// ErrNotPlaced is wrapped by Place errors that guarantee the broker created no
// order: the intent was refused before it was sent, or the broker rejected the
// request itself. Any other Place error leaves it unknown whether the order
// exists.
var ErrNotPlaced = errors.New("orders: order not placed")

// OrderBook is implemented by brokers that can list the trading day's orders.
// The manager needs it to reconcile recorded intents on restart.
type OrderBook interface {
	// Orders returns the latest state of every order the broker holds for the
	// trading day, one update per order, with ClientID set to the order tag.
	Orders(ctx context.Context) ([]domain.OrderUpdate, error)
}

// ClientIDPrefix starts every client id, so the system's own orders can be told
// apart from orders placed by hand.
const ClientIDPrefix = "jt"

// ClientID returns the deterministic client id for a decision cycle. It is
// sent as the Kite order tag, which allows at most 20 alphanumeric characters.
func ClientID(cycleID string) string {
	sum := sha256.Sum256([]byte(cycleID))
	return ClientIDPrefix + hex.EncodeToString(sum[:])[:18]
}
