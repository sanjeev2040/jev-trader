// Package domain holds the small value types shared across jev-trader's
// packages. It has no logic and imports only the standard library.
package domain

import "time"

// Instrument identifies a tradable instrument or index.
type Instrument struct {
	Token    uint32 // Kite instrument token
	Exchange string // e.g. "NSE"
	Symbol   string // trading symbol, e.g. "INFY"
}

// Bar is a closed OHLCV bar covering [Start, End).
type Bar struct {
	Instrument Instrument
	Interval   time.Duration
	Start      time.Time
	End        time.Time
	Open       float64
	High       float64
	Low        float64
	Close      float64
	Volume     int64
}

// Position is the open position in one instrument. Quantity is signed:
// positive is long, negative is short, zero is flat.
type Position struct {
	Instrument Instrument
	Quantity   int64
	AvgPrice   float64
}

// OrderIntent is an order the risk gate has approved for placement.
type OrderIntent struct {
	CycleID    string // Jev decision cycle or risk-driven exit that produced the order
	Instrument Instrument
	Side       Action  // ActionBuy or ActionSell
	Quantity   int64   // shares, always > 0
	RefPrice   float64 // last known price when approved; 0 if unknown
	ReduceOnly bool    // the order only reduces an open position
}

// Fill is a partial or full execution of an order.
type Fill struct {
	ClientID   string
	Instrument Instrument
	Side       Action // ActionBuy or ActionSell
	Quantity   int64  // shares, always > 0
	Price      float64
	Charges    float64 // brokerage, taxes and fees for this fill
	Time       time.Time
}
