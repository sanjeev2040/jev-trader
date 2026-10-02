package store

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

const (
	// maxReceiveLag bounds how long after its exchange time a tick is received.
	maxReceiveLag = 5 * time.Minute
	// maxBarInterval is the longest bar interval bars.Builder builds.
	maxBarInterval = 30 * time.Minute
)

// Query selects stored rows by event time and instrument. Zero values mean
// no bound and every instrument.
type Query struct {
	From, To time.Time // half-open [From, To): tick exchange time, or bar start
	Tokens   []uint32  // instrument tokens; empty means all
}

// BarQuery selects stored bars.
type BarQuery struct {
	Query
	Interval time.Duration // zero means every interval
}

func (q Query) match(token uint32, ts time.Time) bool {
	if !q.From.IsZero() && ts.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !ts.Before(q.To) {
		return false
	}
	return len(q.Tokens) == 0 || slices.Contains(q.Tokens, token)
}

// ScanTicks calls fn for every stored tick matching q, in the order the ticks
// were recorded, so replay feeds bars.Builder exactly what the live run saw. It
// stops at the first error fn returns and returns it.
func ScanTicks(root string, q Query, fn func(domain.Tick) error) error {
	// A tick is filed under its receive time, which can trail its exchange time
	// by up to maxReceiveLag.
	return scan(root, KindTicks, "ticks", strconv.Itoa(TickSchemaVersion), q.From, widen(q.To, maxReceiveLag), func(rows []TickRow) error {
		for _, r := range rows {
			t := rowToTick(r)
			ts := t.ExchangeTime
			if ts.IsZero() {
				ts = t.ReceivedAt
			}
			if !q.match(t.InstrumentToken, ts) {
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
		}
		return nil
	})
}

// ScanBars calls fn for every stored bar of kind matching q, ordered by end
// time, then interval, then instrument token: the order bars.Builder emits.
func ScanBars(root, kind string, q BarQuery, fn func(domain.Bar) error) error {
	if kind != KindBars && kind != KindBackfill {
		return fmt.Errorf("store: %q is not a bar kind", kind)
	}
	// A bar is filed under its end, at most maxBarInterval after its start.
	return scan(root, kind, BarSchemaName, barSchemaVersion, q.From, widen(q.To, maxBarInterval), func(rows []barRow) error {
		bars := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			b := rowToBar(r)
			if (q.Interval == 0 || b.Interval == q.Interval) && q.match(b.Instrument.Token, b.Start) {
				bars = append(bars, b)
			}
		}
		slices.SortStableFunc(bars, func(a, b domain.Bar) int {
			if c := a.End.Compare(b.End); c != 0 {
				return c
			}
			if c := cmp.Compare(a.Interval, b.Interval); c != 0 {
				return c
			}
			return cmp.Compare(a.Instrument.Token, b.Instrument.Token)
		})
		for _, b := range bars {
			if err := fn(b); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReadBars returns every stored bar of kind matching q, in ScanBars order.
func ReadBars(root, kind string, q BarQuery) ([]domain.Bar, error) {
	var out []domain.Bar
	err := ScanBars(root, kind, q, func(b domain.Bar) error {
		out = append(out, b)
		return nil
	})
	return out, err
}

// widen extends an upper bound by d; zero stays unbounded.
func widen(to time.Time, d time.Duration) time.Time {
	if to.IsZero() {
		return to
	}
	return to.Add(d)
}

// scan reads the files of kind one window at a time, in time order, and
// passes each window's rows to fn. Only date partitions that can hold rows in
// [from, to) are opened. A missing kind directory holds no rows.
func scan[R any](root, kind, schema, version string, from, to time.Time, fn func([]R) error) error {
	dates, err := os.ReadDir(filepath.Join(root, kind))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	var lo, hi string
	if !from.IsZero() {
		lo = from.In(ist).Format(time.DateOnly)
	}
	if !to.IsZero() {
		hi = to.Add(-1).In(ist).Format(time.DateOnly)
	}
	for _, d := range dates { // ReadDir sorts by name, so dates ascend
		date, ok := strings.CutPrefix(d.Name(), "date=")
		if !d.IsDir() || !ok || (lo != "" && date < lo) || (hi != "" && date > hi) {
			continue
		}
		dir := filepath.Join(root, kind, d.Name())
		windows, err := windowFiles(dir, kind)
		if err != nil {
			return err
		}
		for _, files := range windows {
			var rows []R
			for _, path := range files {
				rows, err = readFile(path, schema, version, rows)
				if err != nil {
					return err
				}
			}
			if err := fn(rows); err != nil {
				return err
			}
		}
	}
	return nil
}

// windowFiles groups a date directory's published files by window, in window
// then sequence order. Files are named <kind>-<window>[-<seq>].parquet; the
// window is HHMMSS for ticks and HHMM for bars. Hidden temporary files and
// other names are skipped.
func windowFiles(dir, kind string) ([][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	type file struct {
		window string
		seq    int
		path   string
	}
	var files []file
	for _, e := range entries {
		name := e.Name()
		rest, ok := strings.CutPrefix(name, kind+"-")
		rest, ok2 := strings.CutSuffix(rest, ".parquet")
		if e.IsDir() || !ok || !ok2 {
			continue
		}
		window, seq, _ := strings.Cut(rest, "-")
		n := 0
		if seq != "" {
			if n, err = strconv.Atoi(seq); err != nil {
				continue
			}
		}
		if window == "" || strings.Trim(window, "0123456789") != "" {
			continue
		}
		files = append(files, file{window, n, filepath.Join(dir, name)})
	}
	slices.SortFunc(files, func(a, b file) int {
		if c := cmp.Compare(a.window, b.window); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	var groups [][]string
	for i, f := range files {
		if i == 0 || f.window != files[i-1].window {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], f.path)
	}
	return groups, nil
}

// readFile appends the rows of one file to rows after checking its schema
// version.
func readFile[R any](path, schema, version string, rows []R) ([]R, error) {
	f, err := os.Open(path)
	if err != nil {
		return rows, fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return rows, fmt.Errorf("store: %w", err)
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return rows, fmt.Errorf("store: open %s: %w", path, err)
	}
	gotSchema, _ := pf.Lookup(MetaSchema)
	gotVersion, _ := pf.Lookup(MetaSchemaVersion)
	if gotSchema != schema || gotVersion != version {
		return rows, fmt.Errorf("store: %s has schema %q v%q, want %q v%q", path, gotSchema, gotVersion, schema, version)
	}
	r := parquet.NewGenericReader[R](pf)
	defer r.Close()
	buf := make([]R, 1024)
	for {
		n, err := r.Read(buf)
		rows = append(rows, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return rows, fmt.Errorf("store: read %s: %w", path, err)
		}
	}
}

// rowToTick converts a stored tick back to a domain tick. Depth lists shorter
// than the book hold zero levels for the rest.
func rowToTick(r TickRow) domain.Tick {
	t := domain.Tick{
		InstrumentToken:   uint32(r.InstrumentToken),
		Symbol:            r.Symbol,
		ExchangeTime:      utcOrZero(r.ExchangeTS),
		ReceivedAt:        utcOrZero(r.ReceivedTS),
		LastTradeTime:     utcOrZero(r.LastTradeTS),
		LastPrice:         r.LastPrice,
		LastQuantity:      r.LastQuantity,
		AveragePrice:      r.AveragePrice,
		Volume:            r.Volume,
		TotalBuyQuantity:  r.TotalBuyQty,
		TotalSellQuantity: r.TotalSellQty,
		Open:              r.Open,
		High:              r.High,
		Low:               r.Low,
		Close:             r.Close,
		OI:                r.OI,
	}
	for i := range t.Bids {
		if i < len(r.BidPrice) && i < len(r.BidQuantity) && i < len(r.BidOrders) {
			t.Bids[i] = domain.DepthLevel{Price: r.BidPrice[i], Quantity: r.BidQuantity[i], Orders: r.BidOrders[i]}
		}
		if i < len(r.AskPrice) && i < len(r.AskQuantity) && i < len(r.AskOrders) {
			t.Asks[i] = domain.DepthLevel{Price: r.AskPrice[i], Quantity: r.AskQuantity[i], Orders: r.AskOrders[i]}
		}
	}
	return t
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
