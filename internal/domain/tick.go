package domain

import "time"

// DepthLevel is one price level of the order book.
type DepthLevel struct {
	Price    float64
	Quantity int64
	Orders   int64
}

// Tick is one market-data update for one instrument.
//
// Prices are in rupees. Fields the subscription mode does not carry are left
// at their zero value.
type Tick struct {
	InstrumentToken uint32
	Symbol          string // "EXCHANGE:SYMBOL"; empty if the token is not in the watchlist

	ExchangeTime  time.Time // exchange timestamp of the packet; zero if not sent
	ReceivedAt    time.Time // local receive time
	LastTradeTime time.Time // zero if not sent

	LastPrice         float64
	LastQuantity      int64
	AveragePrice      float64
	Volume            int64 // cumulative traded volume for the day
	TotalBuyQuantity  int64
	TotalSellQuantity int64

	// Day OHLC. Close is the previous session's close.
	Open, High, Low, Close float64

	OI int64 // open interest; zero for cash equities

	Bids [5]DepthLevel // best bid first
	Asks [5]DepthLevel // best ask first
}
