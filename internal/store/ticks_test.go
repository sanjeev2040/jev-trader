package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// at returns a time on 2026-10-02 in IST.
func at(hh, mm, ss int) time.Time {
	return time.Date(2026, 10, 2, hh, mm, ss, 0, ist)
}

func tickAt(token uint32, recv time.Time) domain.Tick {
	t := domain.Tick{
		InstrumentToken: token, Symbol: "NSE:INFY",
		ExchangeTime: recv.Add(-time.Second).UTC(), ReceivedAt: recv.UTC(),
		LastPrice: 1500.5, LastQuantity: 3, AveragePrice: 1499, Volume: 1000,
		TotalBuyQuantity: 10, TotalSellQuantity: 20,
		Open: 1490, High: 1510, Low: 1480, Close: 1495,
	}
	t.Bids[0] = domain.DepthLevel{Price: 1500.45, Quantity: 7, Orders: 2}
	t.Asks[1] = domain.DepthLevel{Price: 1500.6, Quantity: 4, Orders: 1}
	return t
}

// files lists every file under root, relative to it.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestTickWriterRoundTrip(t *testing.T) {
	root := t.TempDir()
	w := NewTickWriter(root, 0)
	in := tickAt(408065, at(9, 15, 1))
	noExch := tickAt(2953217, at(9, 15, 2))
	noExch.ExchangeTime = time.Time{}
	noExch.LastTradeTime = time.Time{}
	for _, tk := range []domain.Tick{in, noExch} {
		if err := w.Write(tk); err != nil {
			t.Fatal(err)
		}
	}
	// The open file is hidden until it is complete.
	if got := files(t, root); len(got) != 1 || !strings.HasPrefix(filepath.Base(got[0]), ".") || !strings.HasSuffix(got[0], ".tmp") {
		t.Fatalf("open file = %v, want one hidden .tmp file", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "ticks", "date=2026-10-02", "ticks-091500.parquet")
	if got := files(t, root); !reflect.DeepEqual(got, []string{"ticks/date=2026-10-02/ticks-091500.parquet"}) {
		t.Fatalf("files = %v", got)
	}
	rows, err := parquet.ReadFile[TickRow](path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2", len(rows))
	}
	want := NewTickRow(in)
	got := rows[0]
	if !got.ExchangeTS.Equal(want.ExchangeTS) || !got.ReceivedTS.Equal(want.ReceivedTS) || !got.LastTradeTS.Equal(want.LastTradeTS) {
		t.Errorf("times = %v %v %v, want %v %v %v", got.ExchangeTS, got.ReceivedTS, got.LastTradeTS, want.ExchangeTS, want.ReceivedTS, want.LastTradeTS)
	}
	got.ExchangeTS, got.ReceivedTS, got.LastTradeTS = want.ExchangeTS, want.ReceivedTS, want.LastTradeTS
	if !reflect.DeepEqual(got, want) {
		t.Errorf("row =\n%+v\nwant\n%+v", got, want)
	}
	if !rows[1].ExchangeTS.IsZero() || !rows[1].LastTradeTS.IsZero() {
		t.Errorf("missing times read back as %v and %v, want zero", rows[1].ExchangeTS, rows[1].LastTradeTS)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	pf, err := parquet.OpenFile(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := pf.Lookup(MetaSchema); v != "ticks" {
		t.Errorf("schema metadata = %q", v)
	}
	if v, _ := pf.Lookup(MetaSchemaVersion); v != "1" {
		t.Errorf("schema version metadata = %q", v)
	}
	for _, col := range []string{"exchange_ts", "last_trade_ts"} {
		if c, ok := pf.Schema().Lookup(col); !ok || !c.Node.Optional() {
			t.Errorf("column %s should be optional (null when not sent)", col)
		}
	}
}

func TestTickWriterRotatesByWindowAndDay(t *testing.T) {
	root := t.TempDir()
	w := NewTickWriter(root, 15*time.Minute)
	for _, recv := range []time.Time{
		at(9, 14, 59),
		at(9, 15, 0), // new window
		at(9, 29, 59),
		at(9, 29, 58),                       // slightly earlier: stays in the open file
		at(23, 59, 59),                      // new window
		at(23, 59, 59).Add(2 * time.Second), // next IST day
	} {
		if err := w.Write(tickAt(1, recv)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ticks/date=2026-10-02/ticks-090000.parquet",
		"ticks/date=2026-10-02/ticks-091500.parquet",
		"ticks/date=2026-10-02/ticks-234500.parquet",
		"ticks/date=2026-10-03/ticks-000000.parquet",
	}
	if got := files(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("files =\n%v\nwant\n%v", got, want)
	}
	rows, err := parquet.ReadFile[TickRow](filepath.Join(root, want[1]))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("09:15 window has %d rows, want 3", len(rows))
	}
}

func TestTickWriterRotateCompletesIdleFile(t *testing.T) {
	root := t.TempDir()
	w := NewTickWriter(root, 15*time.Minute)
	if err := w.Write(tickAt(1, at(10, 0, 5))); err != nil {
		t.Fatal(err)
	}
	if err := w.Rotate(at(10, 14, 59)); err != nil {
		t.Fatal(err)
	}
	if got := files(t, root); len(got) != 1 || !strings.HasSuffix(got[0], ".tmp") {
		t.Fatalf("rotated early: %v", got)
	}
	if err := w.Rotate(at(10, 15, 0)); err != nil {
		t.Fatal(err)
	}
	if got := files(t, root); !reflect.DeepEqual(got, []string{"ticks/date=2026-10-02/ticks-100000.parquet"}) {
		t.Fatalf("files = %v", got)
	}
	if err := w.Rotate(at(11, 0, 0)); err != nil {
		t.Errorf("rotate with no open file: %v", err)
	}
}

func TestTickWriterRestartInSameWindowDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	for range 2 {
		w := NewTickWriter(root, 15*time.Minute)
		if err := w.Write(tickAt(1, at(9, 20, 0))); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"ticks/date=2026-10-02/ticks-091500-1.parquet",
		"ticks/date=2026-10-02/ticks-091500.parquet",
	}
	if got := files(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestTickWriterRejectsTickWithoutTime(t *testing.T) {
	if err := NewTickWriter(t.TempDir(), 0).Write(domain.Tick{InstrumentToken: 1}); err == nil {
		t.Error("expected an error")
	}
}

func TestRecordTicks(t *testing.T) {
	root := t.TempDir()
	in := make(chan domain.Tick, 3)
	in <- tickAt(1, at(9, 15, 0))
	in <- tickAt(2, at(9, 15, 1))
	close(in)
	if err := RecordTicks(context.Background(), in, NewTickWriter(root, 0), nil); err != nil {
		t.Fatal(err)
	}
	rows, err := parquet.ReadFile[TickRow](filepath.Join(root, "ticks", "date=2026-10-02", "ticks-091500.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2", len(rows))
	}
}

func TestRecordTicksDrainsOnCancel(t *testing.T) {
	root := t.TempDir()
	in := make(chan domain.Tick, 3)
	in <- tickAt(1, at(9, 15, 0))
	in <- tickAt(2, at(9, 15, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RecordTicks(ctx, in, NewTickWriter(root, 0), nil); err != nil {
		t.Fatal(err)
	}
	rows, err := parquet.ReadFile[TickRow](filepath.Join(root, "ticks", "date=2026-10-02", "ticks-091500.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want the 2 already buffered", len(rows))
	}
}
