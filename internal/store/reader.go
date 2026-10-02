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
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/domain"
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
	// A tick is filed under its receive time, which follows its exchange time,
	// so a tick inside the query can be filed up to a day after its upper bound.
	to := q.To
	if !to.IsZero() {
		to = to.Add(24 * time.Hour)
	}
	return scan(root, KindTicks, TickSchemaVersion, q.From, to, func(rows []tickRow) error {
		for _, r := range rows {
			t := rowToTick(r)
			if !q.match(t.InstrumentToken, t.ExchangeTime) {
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
	// A bar is filed under its end, which can be up to a day after a bar start
	// inside the query, so widen the upper bound when choosing dates.
	to := q.To
	if !to.IsZero() {
		to = to.Add(24 * time.Hour)
	}
	return scan(root, kind, BarSchemaVersion, q.From, to, func(rows []barRow) error {
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

// scan reads the files of kind one window at a time, in time order, and
// passes each window's rows to fn. Only date partitions that can hold rows in
// [from, to) are opened. A missing kind directory holds no rows.
func scan[R any](root, kind, schema string, from, to time.Time, fn func([]R) error) error {
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
		hi = to.In(ist).Format(time.DateOnly)
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
				rows, err = readFile(path, schema, rows)
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
// then sequence order. Hidden temporary files are skipped.
func windowFiles(dir, kind string) ([][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var groups [][]string
	last := ""
	for _, e := range entries { // sorted: <kind>-HHMM-NNN.parquet
		name := e.Name()
		rest, ok := strings.CutPrefix(name, kind+"-")
		if e.IsDir() || !ok || !strings.HasSuffix(name, ".parquet") || len(rest) < 4 {
			continue
		}
		path := filepath.Join(dir, name)
		if window := rest[:4]; window != last || len(groups) == 0 {
			groups = append(groups, []string{path})
			last = window
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], path)
		}
	}
	return groups, nil
}

// readFile appends the rows of one file to rows after checking its schema
// version.
func readFile[R any](path, schema string, rows []R) ([]R, error) {
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
	if got, _ := pf.Lookup(SchemaKey); got != schema {
		return rows, fmt.Errorf("store: %s has schema %q, want %q", path, got, schema)
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
