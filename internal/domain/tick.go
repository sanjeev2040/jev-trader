package domain

import "time"

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
