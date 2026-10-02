// Package store writes and reads ticks and bars as date-partitioned Parquet.
//
// Layout under a data root:
//
//	<root>/<kind>/date=YYYY-MM-DD/<kind>-HHMM-NNN.parquet
//
// The date and HHMM are the IST date and the start of the 15-minute window the
// rows fall in: a tick by its receive time, so files hold ticks in the order
// they arrived, and a bar by its end. NNN distinguishes files of one window written by different
// runs. A file is written under a hidden temporary name and renamed when it is
// closed, so readers never see a partial file and a crash loses at most the
// file that was open.
//
// The row schemas below are the contract with Python analysis tools. Any change
// to them must bump the matching schema version.
package store

import (
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Kinds of data, each stored in its own directory under the data root.
const (
	KindTicks = "ticks"
	// KindBars holds bars built from recorded ticks.
	KindBars = "bars"
	// KindBackfill holds bars fetched from Kite historical candles. They can
	// differ from tick-built bars, so they are kept apart and backtests on them
	// are labelled as such.
	KindBackfill = "bars_backfill"
)

// Schema versions, stored in each file's key-value metadata under SchemaKey.
const (
	SchemaKey         = "jevtrader.schema"
	KindKey           = "jevtrader.kind"
	TickSchemaVersion = "ticks/v1"
	BarSchemaVersion  = "bars/v1"
)

// RotateEvery is the width of a file's time window.
const RotateEvery = 15 * time.Minute

// ist is the exchange timezone used for partitioning.
var ist = time.FixedZone("IST", 5*3600+30*60)

// tickRow is the Parquet schema of a tick (TickSchemaVersion). Times are UTC
// nanoseconds; an unknown time is stored as 0 (the Unix epoch).
type tickRow struct {
	InstrumentToken uint32  `parquet:"instrument_token"`
	ExchangeTime    int64   `parquet:"exchange_time,timestamp(nanosecond)"`
	LastTradeTime   int64   `parquet:"last_trade_time,timestamp(nanosecond)"`
	ReceiveTime     int64   `parquet:"receive_time,timestamp(nanosecond)"`
	LastPrice       float64 `parquet:"last_price"`
	LastQuantity    uint32  `parquet:"last_quantity"`
	AveragePrice    float64 `parquet:"average_price"`
	VolumeTraded    uint64  `parquet:"volume_traded"`
	TotalBuyQty     uint64  `parquet:"total_buy_qty"`
	TotalSellQty    uint64  `parquet:"total_sell_qty"`
	OI              uint64  `parquet:"oi"`
}

// barRow is the Parquet schema of a bar (BarSchemaVersion). The bar covers
// [start, end).
type barRow struct {
	InstrumentToken uint32  `parquet:"instrument_token"`
	Exchange        string  `parquet:"exchange"`
	Symbol          string  `parquet:"symbol"`
	IntervalSeconds int32   `parquet:"interval_seconds"`
	Start           int64   `parquet:"start,timestamp(nanosecond)"`
	End             int64   `parquet:"end,timestamp(nanosecond)"`
	Open            float64 `parquet:"open"`
	High            float64 `parquet:"high"`
	Low             float64 `parquet:"low"`
	Close           float64 `parquet:"close"`
	Volume          int64   `parquet:"volume"`
}

func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func tickToRow(t domain.Tick) tickRow {
	return tickRow{
		InstrumentToken: t.InstrumentToken,
		ExchangeTime:    toNanos(t.ExchangeTime),
		LastTradeTime:   toNanos(t.LastTradeTime),
		ReceiveTime:     toNanos(t.ReceiveTime),
		LastPrice:       t.LastPrice,
		LastQuantity:    t.LastQuantity,
		AveragePrice:    t.AveragePrice,
		VolumeTraded:    t.VolumeTraded,
		TotalBuyQty:     t.TotalBuyQty,
		TotalSellQty:    t.TotalSellQty,
		OI:              t.OI,
	}
}

func rowToTick(r tickRow) domain.Tick {
	return domain.Tick{
		InstrumentToken: r.InstrumentToken,
		ExchangeTime:    fromNanos(r.ExchangeTime),
		LastTradeTime:   fromNanos(r.LastTradeTime),
		ReceiveTime:     fromNanos(r.ReceiveTime),
		LastPrice:       r.LastPrice,
		LastQuantity:    r.LastQuantity,
		AveragePrice:    r.AveragePrice,
		VolumeTraded:    r.VolumeTraded,
		TotalBuyQty:     r.TotalBuyQty,
		TotalSellQty:    r.TotalSellQty,
		OI:              r.OI,
	}
}

func barToRow(b domain.Bar) barRow {
	return barRow{
		InstrumentToken: b.Instrument.Token,
		Exchange:        b.Instrument.Exchange,
		Symbol:          b.Instrument.Symbol,
		IntervalSeconds: int32(b.Interval / time.Second),
		Start:           toNanos(b.Start),
		End:             toNanos(b.End),
		Open:            b.Open,
		High:            b.High,
		Low:             b.Low,
		Close:           b.Close,
		Volume:          b.Volume,
	}
}

func rowToBar(r barRow) domain.Bar {
	return domain.Bar{
		Instrument: domain.Instrument{
			Token:    r.InstrumentToken,
			Exchange: r.Exchange,
			Symbol:   r.Symbol,
		},
		Interval: time.Duration(r.IntervalSeconds) * time.Second,
		Start:    fromNanos(r.Start),
		End:      fromNanos(r.End),
		Open:     r.Open,
		High:     r.High,
		Low:      r.Low,
		Close:    r.Close,
		Volume:   r.Volume,
	}
}

// barTime is the instant a bar is partitioned by: the last instant it covers,
// so a bar is filed with the window and date in which it closes and files are
// ordered by bar end.
func barTime(b domain.Bar) time.Time { return b.End.Add(-1) }
