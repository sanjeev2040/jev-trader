// Package domain holds the small value types shared across jev-trader.
//
// It has no logic and imports only the standard library, so every other
// package can depend on it without creating cycles (reports/0002-architecture.md).
package domain

import "time"

// Instrument identifies one tradable instrument on an exchange.
type Instrument struct {
	Token    uint32 // Kite instrument token
	Exchange string // e.g. "NSE"
	Symbol   string // trading symbol, e.g. "INFY"
}

// Tick is one market-data update for an instrument.
type Tick struct {
	InstrumentToken uint32
	ExchangeTime    time.Time // exchange timestamp of the packet; event time for bars
	LastTradeTime   time.Time // time of the last trade, zero if unknown
	ReceiveTime     time.Time // local time the packet was received
	LastPrice       float64
	LastQuantity    uint32  // quantity of the last trade
	AveragePrice    float64 // volume-weighted average traded price for the day
	VolumeTraded    uint64  // cumulative traded volume for the day
	TotalBuyQty     uint64  // total pending buy quantity
	TotalSellQty    uint64  // total pending sell quantity
	OI              uint64  // open interest, zero for equities
}

// Bar is an OHLCV bar covering [Start, Start+Interval) in event time.
type Bar struct {
	InstrumentToken uint32
	Interval        time.Duration
	Start           time.Time
	Open            float64
	High            float64
	Low             float64
	Close           float64
	Volume          uint64 // volume traded within the bar
	Ticks           uint32 // number of ticks aggregated, zero when not built from ticks
}
