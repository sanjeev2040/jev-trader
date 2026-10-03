package orders

import (
	"errors"
	"fmt"
	"math"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// ErrInvalidIntent is wrapped by every ValidateIntent error.
var ErrInvalidIntent = errors.New("orders: invalid intent")

// MarketProtectionPct is the market protection, in percent, sent with every
// market order. Kite rejects market orders whose protection is zero.
const MarketProtectionPct = 2

// PriceTick is the NSE equity price tick. Limit prices are rounded to it.
const PriceTick = 0.05

// Spec is how an approved intent is sent to a broker.
type Spec struct {
	Type             domain.OrderType
	LimitPrice       float64 // set for a LIMIT order
	MarketProtection float64 // set for a MARKET order
}

// SpecFor derives the order type and prices for an intent. An entry with a
// known reference price is a limit order at that price. A reduce-only exit
// (square-off, loss limit, kill switch) must get out, so it is a market order
// with market protection, as is any order without a reference price.
func SpecFor(o domain.OrderIntent) Spec {
	if o.RefPrice > 0 && !o.ReduceOnly {
		return Spec{Type: domain.OrderTypeLimit, LimitPrice: roundToTick(o.RefPrice)}
	}
	return Spec{Type: domain.OrderTypeMarket, MarketProtection: MarketProtectionPct}
}

// roundToTick rounds p to the nearest tick, in whole paise so float error
// does not leave a price off the grid.
func roundToTick(p float64) float64 {
	return math.Round(math.Round(p/PriceTick)*PriceTick*100) / 100
}

// ValidateIntent checks that an intent is complete and placeable. Brokers call
// it too, so a bad intent never reaches the wire.
func ValidateIntent(o domain.OrderIntent) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidIntent, fmt.Sprintf(format, args...))
	}
	switch {
	case o.CycleID == "":
		return invalid("missing cycle id")
	case o.Instrument.Exchange == "" || o.Instrument.Symbol == "":
		return invalid("instrument needs an exchange and a trading symbol")
	case o.Side != domain.ActionBuy && o.Side != domain.ActionSell:
		return invalid("side %q is not BUY or SELL", o.Side)
	case o.Quantity <= 0:
		return invalid("quantity %d is not positive", o.Quantity)
	case math.IsNaN(o.RefPrice) || math.IsInf(o.RefPrice, 0) || o.RefPrice < 0:
		return invalid("reference price %v is not a finite non-negative number", o.RefPrice)
	}
	if s := SpecFor(o); s.Type == domain.OrderTypeLimit && !(s.LimitPrice > 0) {
		return invalid("reference price %v rounds to a zero limit price", o.RefPrice)
	}
	return nil
}
