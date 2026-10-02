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
