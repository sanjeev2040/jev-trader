// Package store owns the on-disk Parquet formats for ticks and bars.
//
// The Parquet schemas are the contract with Python analysis, so every
// schema change bumps its version, which each file records in its key-value
// metadata.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

const (
	// TickSchemaVersion is the version of the tick Parquet schema.
	TickSchemaVersion = 1

	// MetaSchema and MetaSchemaVersion are the Parquet key-value metadata
	// keys naming a file's schema and its version.
	MetaSchema        = "jevtrader.schema"
	MetaSchemaVersion = "jevtrader.schema_version"

	// DefaultRotation is how long a tick file stays open. A crash loses at
	// most the open file.
	DefaultRotation = 15 * time.Minute

	// flushRows bounds the rows buffered in memory before a row group is
	// written.
	flushRows = 50_000
)

// ist partitions files by Indian trading day.
var ist = time.FixedZone("IST", 5*3600+1800)

// TickRow is one tick as stored in Parquet (schema version 1). Times are
// UTC-adjusted nanosecond timestamps; exchange_ts and last_trade_ts are null
// when the feed did not send them. Depth lists hold five levels, best first.
type TickRow struct {
	InstrumentToken int64     `parquet:"instrument_token"`
	Symbol          string    `parquet:"symbol"`
	ExchangeTS      time.Time `parquet:"exchange_ts,optional,timestamp(nanosecond)"`
	ReceivedTS      time.Time `parquet:"received_ts,timestamp(nanosecond)"`
	LastTradeTS     time.Time `parquet:"last_trade_ts,optional,timestamp(nanosecond)"`
	LastPrice       float64   `parquet:"last_price"`
	LastQuantity    int64     `parquet:"last_quantity"`
	AveragePrice    float64   `parquet:"average_price"`
	Volume          int64     `parquet:"volume"`
	TotalBuyQty     int64     `parquet:"total_buy_quantity"`
	TotalSellQty    int64     `parquet:"total_sell_quantity"`
	Open            float64   `parquet:"open"`
	High            float64   `parquet:"high"`
	Low             float64   `parquet:"low"`
	Close           float64   `parquet:"close"`
	OI              int64     `parquet:"oi"`
	BidPrice        []float64 `parquet:"bid_price,list"`
	BidQuantity     []int64   `parquet:"bid_quantity,list"`
	BidOrders       []int64   `parquet:"bid_orders,list"`
	AskPrice        []float64 `parquet:"ask_price,list"`
	AskQuantity     []int64   `parquet:"ask_quantity,list"`
	AskOrders       []int64   `parquet:"ask_orders,list"`
}

// NewTickRow converts a domain tick to its stored form.
func NewTickRow(t domain.Tick) TickRow {
	r := TickRow{
		InstrumentToken: int64(t.InstrumentToken),
		Symbol:          t.Symbol,
		ExchangeTS:      t.ExchangeTime,
		ReceivedTS:      t.ReceivedAt,
		LastTradeTS:     t.LastTradeTime,
		LastPrice:       t.LastPrice,
		LastQuantity:    t.LastQuantity,
		AveragePrice:    t.AveragePrice,
		Volume:          t.Volume,
		TotalBuyQty:     t.TotalBuyQuantity,
		TotalSellQty:    t.TotalSellQuantity,
		Open:            t.Open,
		High:            t.High,
		Low:             t.Low,
		Close:           t.Close,
		OI:              t.OI,
	}
	n := len(t.Bids)
	r.BidPrice, r.BidQuantity, r.BidOrders = make([]float64, n), make([]int64, n), make([]int64, n)
	r.AskPrice, r.AskQuantity, r.AskOrders = make([]float64, n), make([]int64, n), make([]int64, n)
	for i := range n {
		b, a := t.Bids[i], t.Asks[i]
		r.BidPrice[i], r.BidQuantity[i], r.BidOrders[i] = b.Price, b.Quantity, b.Orders
		r.AskPrice[i], r.AskQuantity[i], r.AskOrders[i] = a.Price, a.Quantity, a.Orders
	}
	return r
}

// TickWriter appends ticks to Parquet files under
// root/ticks/date=YYYY-MM-DD/ (IST date). Each file covers one rotation
// window, aligned to the wall clock, and is named after the window's start
// (ticks-HHMMSS.parquet). A file is written as a hidden ".tmp" file and
// renamed into place only once complete, so readers never see a partial
// file.
//
// A tick's window is taken from its local receive time, which only moves
// forward in practice; a tick stamped before the open window is written to
// the open file. TickWriter is not safe for concurrent use.
type TickWriter struct {
	root     string
	rotation time.Duration
	cur      *tickFile
}

type tickFile struct {
	f          *os.File
	w          *parquet.GenericWriter[TickRow]
	tmp, final string
	end        time.Time
	unflushed  int
}

// NewTickWriter returns a writer rooted at root. rotation <= 0 means
// DefaultRotation.
func NewTickWriter(root string, rotation time.Duration) *TickWriter {
	if rotation <= 0 {
		rotation = DefaultRotation
	}
	return &TickWriter{root: root, rotation: rotation}
}

// Write appends one tick, rotating to a new file when the tick falls past
// the open window.
func (w *TickWriter) Write(t domain.Tick) error {
	at := t.ReceivedAt
	if at.IsZero() {
		at = t.ExchangeTime
	}
	if at.IsZero() {
		return errors.New("tick writer: tick has no receive or exchange time")
	}
	if w.cur != nil && !at.Before(w.cur.end) {
		if err := w.closeCurrent(); err != nil {
			return err
		}
	}
	if w.cur == nil {
		if err := w.open(at); err != nil {
			return err
		}
	}
	if _, err := w.cur.w.Write([]TickRow{NewTickRow(t)}); err != nil {
		return fmt.Errorf("tick writer: %w", err)
	}
	w.cur.unflushed++
	if w.cur.unflushed >= flushRows {
		if err := w.cur.w.Flush(); err != nil {
			return fmt.Errorf("tick writer: %w", err)
		}
		w.cur.unflushed = 0
	}
	return nil
}

// Rotate closes the open file if its window ended at or before now. Call it
// periodically so a quiet feed still completes files on time.
func (w *TickWriter) Rotate(now time.Time) error {
	if w.cur == nil || now.Before(w.cur.end) {
		return nil
	}
	return w.closeCurrent()
}

// Close completes the open file, if any.
func (w *TickWriter) Close() error {
	if w.cur == nil {
		return nil
	}
	return w.closeCurrent()
}

func (w *TickWriter) open(at time.Time) error {
	// Windows are aligned to IST midnight and never cross into the next
	// day's partition.
	local := at.In(ist)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, ist)
	start := day.Add(local.Sub(day).Truncate(w.rotation))
	end := start.Add(w.rotation)
	if next := day.AddDate(0, 0, 1); end.After(next) {
		end = next
	}

	dir := filepath.Join(w.root, "ticks", "date="+local.Format(time.DateOnly))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tick writer: %w", err)
	}
	final, err := freeName(dir, "ticks-"+start.Format("150405"))
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(final)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("tick writer: %w", err)
	}
	pw := parquet.NewGenericWriter[TickRow](f,
		parquet.Compression(&parquet.Zstd),
		parquet.KeyValueMetadata(MetaSchema, "ticks"),
		parquet.KeyValueMetadata(MetaSchemaVersion, strconv.Itoa(TickSchemaVersion)),
	)
	w.cur = &tickFile{f: f, w: pw, tmp: tmp, final: final, end: end}
	return nil
}

// freeName returns dir/base.parquet, or dir/base-N.parquet when a restart
// inside the same window already completed a file with that name.
func freeName(dir, base string) (string, error) {
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name += "-" + strconv.Itoa(i)
		}
		path := filepath.Join(dir, name+".parquet")
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", fmt.Errorf("tick writer: %w", err)
		}
	}
}

func (w *TickWriter) closeCurrent() error {
	c := w.cur
	w.cur = nil
	err := c.w.Close()
	if err == nil {
		err = c.f.Sync()
	}
	if cerr := c.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(c.tmp, c.final)
	}
	if err != nil {
		return fmt.Errorf("tick writer: complete %s: %w", c.final, err)
	}
	return nil
}

// RecordTicks writes every tick from in to w until in is closed or ctx
// ends, rotating files on each value from rotateTick (for example a
// time.Ticker's channel). It closes w before returning.
func RecordTicks(ctx context.Context, in <-chan domain.Tick, w *TickWriter, rotateTick <-chan time.Time) (err error) {
	defer func() {
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}()
	for {
		select {
		case <-ctx.Done():
			// Drain what is already buffered so a clean shutdown loses
			// nothing that reached the recorder.
			for {
				select {
				case t, ok := <-in:
					if !ok {
						return nil
					}
					if err := w.Write(t); err != nil {
						return err
					}
				default:
					return nil
				}
			}
		case t, ok := <-in:
			if !ok {
				return nil
			}
			if err := w.Write(t); err != nil {
				return err
			}
		case now := <-rotateTick:
			if err := w.Rotate(now); err != nil {
				return err
			}
		}
	}
}
