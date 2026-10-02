package orders

import (
	"errors"
	"fmt"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// ErrInvalidIntent is wrapped by every ValidateIntent error.
var ErrInvalidIntent = errors.New("orders: invalid intent")

// ValidateIntent checks that an intent is complete and placeable. Brokers call
// it too, so a bad intent never reaches the wire.
func ValidateIntent(o domain.OrderIntent) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidIntent, fmt.Sprintf(format, args...))
	}
	switch {
	case o.CycleID == "":
		return invalid("missing cycle id")
	case o.ClientID != ClientID(o.CycleID):
		return invalid("client id %q is not derived from cycle id %q", o.ClientID, o.CycleID)
	case o.Instrument.Exchange == "" || o.Instrument.Symbol == "":
		return invalid("instrument needs an exchange and a trading symbol")
	case o.Side != domain.ActionBuy && o.Side != domain.ActionSell:
		return invalid("side %q is not BUY or SELL", o.Side)
	case o.Quantity <= 0:
		return invalid("quantity %d is not positive", o.Quantity)
	}
	switch o.Type {
	case domain.OrderTypeLimit:
		if !(o.LimitPrice > 0) {
			return invalid("limit order needs a positive limit price")
		}
	case domain.OrderTypeMarket:
		if !(o.MarketProtection > 0) {
			return invalid("market order needs non-zero market protection")
		}
	default:
		return invalid("order type %q is not LIMIT or MARKET", o.Type)
	}
	return nil
}
