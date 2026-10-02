package domain

import "time"

// Tick is one market-data update for an instrument.
type Tick struct {
	Token        uint32
	LastPrice    float64
	ExchangeTime time.Time // the exchange timestamp
	ReceivedAt   time.Time // local receive time
}
