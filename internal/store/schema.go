// Package store owns the on-disk Parquet formats for ticks and bars.
//
// Layout under a data root:
//
//	<root>/ticks/date=YYYY-MM-DD/ticks-HHMMSS[-N].parquet   (TickWriter)
//	<root>/<bar kind>/date=YYYY-MM-DD/<kind>-HHMM-NNN.parquet   (BarWriter)
//
// The date is the IST date. Tick files are named after their window's start
// and a restart inside one window adds -N; bar files are named after the start
// of the 15-minute window in which their bars close, and NNN distinguishes
// files of one window written by different runs. A file is written under a
// hidden temporary name and renamed when it is closed, so readers never see a
// partial file and a crash loses at most the file that was open.
//
// The row schemas are the contract with Python analysis tools. Any change to
// one must bump its schema version, which each file records in its key-value
// metadata.
package store

import (
	"strconv"
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

// Bar schema name and version, and the metadata key naming a file's kind.
// The schema keys are MetaSchema and MetaSchemaVersion (see TickWriter).
const (
	BarSchemaName    = "bars"
	BarSchemaVersion = 1
	MetaKind         = "jevtrader.kind"
)

// RotateEvery is the width of a bar file's time window.
const RotateEvery = 15 * time.Minute

// barRow is the Parquet schema of a bar (BarSchemaVersion). Times are UTC
// nanoseconds; the bar covers [start, end).
type barRow struct {
	InstrumentToken int64     `parquet:"instrument_token"`
	Exchange        string    `parquet:"exchange"`
	Symbol          string    `parquet:"symbol"`
	IntervalSeconds int32     `parquet:"interval_seconds"`
	Start           time.Time `parquet:"start,timestamp(nanosecond)"`
	End             time.Time `parquet:"end,timestamp(nanosecond)"`
	Open            float64   `parquet:"open"`
	High            float64   `parquet:"high"`
	Low             float64   `parquet:"low"`
	Close           float64   `parquet:"close"`
	Volume          int64     `parquet:"volume"`
}

func barToRow(b domain.Bar) barRow {
	return barRow{
		InstrumentToken: int64(b.Instrument.Token),
		Exchange:        b.Instrument.Exchange,
		Symbol:          b.Instrument.Symbol,
		IntervalSeconds: int32(b.Interval / time.Second),
		Start:           b.Start,
		End:             b.End,
		Open:            b.Open,
		High:            b.High,
		Low:             b.Low,
		Close:           b.Close,
		Volume:          b.Volume,
	}
}

func rowToBar(r barRow) domain.Bar {
	return domain.Bar{
		Instrument: domain.Instrument{Token: uint32(r.InstrumentToken), Exchange: r.Exchange, Symbol: r.Symbol},
		Interval:   time.Duration(r.IntervalSeconds) * time.Second,
		Start:      r.Start.UTC(),
		End:        r.End.UTC(),
		Open:       r.Open,
		High:       r.High,
		Low:        r.Low,
		Close:      r.Close,
		Volume:     r.Volume,
	}
}

// barTime is the instant a bar is partitioned by: the last instant it covers,
// so a bar is filed with the window and date in which it closes and files are
// ordered by bar end.
func barTime(b domain.Bar) time.Time { return b.End.Add(-1) }

var barSchemaVersion = strconv.Itoa(BarSchemaVersion)
