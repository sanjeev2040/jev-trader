package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/domain"
)

func fixtureBar(token uint32, interval time.Duration, start time.Time, price float64) domain.Bar {
	return domain.Bar{
		Instrument: domain.Instrument{Token: token, Exchange: "NSE", Symbol: "INFY"},
		Interval:   interval, Start: start, End: start.Add(interval),
		Open: price, High: price + 1, Low: price - 1, Close: price + 0.5, Volume: 1234,
	}
}

func equalBars(t *testing.T, got, want []domain.Bar) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d bars, want %d:\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.Start.Equal(w.Start) {
			t.Errorf("bar %d start = %v, want %v", i, g.Start, w.Start)
		}
		if !g.End.Equal(w.End) {
			t.Errorf("bar %d end = %v, want %v", i, g.End, w.End)
		}
		g.Start, w.Start, g.End, w.End = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		if g != w {
			t.Errorf("bar %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeBars(t *testing.T, root, kind string, bars ...domain.Bar) {
	t.Helper()
	w, err := NewBarWriter(root, kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(bars...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBarRoundTripAndLayout(t *testing.T) {
	root := t.TempDir()
	in := []domain.Bar{
		fixtureBar(1, time.Minute, at(9, 15, 0), 100),
		fixtureBar(2, time.Minute, at(9, 15, 0), 200),
		fixtureBar(1, time.Minute, at(9, 29, 0), 101), // ends 09:30: still the 09:15 window
		fixtureBar(1, 5*time.Minute, at(9, 25, 0), 102),
		fixtureBar(1, time.Minute, at(9, 30, 0), 103), // next window
		fixtureBar(1, time.Minute, at(9, 15, 0).AddDate(0, 0, 1), 104),
	}
	writeBars(t, root, KindBars, in...)

	wantFiles := []string{
		"bars/date=2026-10-02/bars-0915-000.parquet",
		"bars/date=2026-10-02/bars-0930-000.parquet",
		"bars/date=2026-10-03/bars-0915-000.parquet",
	}
	if got := listFiles(t, root); !slices.Equal(got, wantFiles) {
		t.Fatalf("files = %v, want %v", got, wantFiles)
	}

	got, err := ReadBars(root, KindBars, BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{in[0], in[1], in[2], in[3], in[4], in[5]})
	if got[0].Start.Location() != time.UTC {
		t.Errorf("start location = %v, want UTC", got[0].Start.Location())
	}
}

func TestReadOrdersByEndThenIntervalThenToken(t *testing.T) {
	root := t.TempDir()
	five := fixtureBar(1, 5*time.Minute, at(9, 15, 0), 1)
	last := fixtureBar(9, time.Minute, at(9, 19, 0), 2)
	first := fixtureBar(3, time.Minute, at(9, 19, 0), 3)
	early := fixtureBar(9, time.Minute, at(9, 16, 0), 4)
	writeBars(t, root, KindBars, five, last, first, early)
	got, err := ReadBars(root, KindBars, BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{early, first, last, five})
}

func TestBarQueryFilters(t *testing.T) {
	root := t.TempDir()
	var all []domain.Bar
	for m := 15; m < 40; m++ {
		all = append(all, fixtureBar(1, time.Minute, at(9, m, 0), float64(m)))
		all = append(all, fixtureBar(2, time.Minute, at(9, m, 0), float64(m)))
	}
	all = append(all, fixtureBar(1, 5*time.Minute, at(9, 20, 0), 1))
	writeBars(t, root, KindBars, all...)

	got, err := ReadBars(root, KindBars, BarQuery{
		Query:    Query{From: at(9, 20, 0), To: at(9, 22, 0), Tokens: []uint32{2}},
		Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{
		fixtureBar(2, time.Minute, at(9, 20, 0), 20),
		fixtureBar(2, time.Minute, at(9, 21, 0), 21),
	})

	got, err = ReadBars(root, KindBars, BarQuery{Interval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{fixtureBar(1, 5*time.Minute, at(9, 20, 0), 1)})

	// Dates entirely outside the range are not read.
	got, err = ReadBars(root, KindBars, BarQuery{Query: Query{From: at(9, 0, 0).AddDate(0, 0, 1)}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing", got, err)
	}
}

func TestOpenFileIsInvisibleUntilRotated(t *testing.T) {
	root := t.TempDir()
	w, err := NewBarWriter(root, KindBars)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureBar(1, time.Minute, at(9, 15, 0), 100)
	if err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadBars(root, KindBars, BarQuery{}); len(got) != 0 {
		t.Fatalf("open file visible: %v", got)
	}
	if err := w.Rotate(at(9, 29, 59)); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadBars(root, KindBars, BarQuery{}); len(got) != 0 {
		t.Fatalf("rotated before window end: %v", got)
	}
	if err := w.Rotate(at(9, 30, 0)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBars(root, KindBars, BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{b})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestartAndCrashLeftovers(t *testing.T) {
	root := t.TempDir()
	a := fixtureBar(1, time.Minute, at(9, 16, 0), 1)
	writeBars(t, root, KindBars, a)

	// A crashed run leaves a temporary file behind; it must be skipped, not reused or read.
	dir := filepath.Join(root, "bars", "date=2026-10-02")
	if err := os.WriteFile(filepath.Join(dir, ".bars-0915-001.parquet.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := fixtureBar(1, time.Minute, at(9, 15, 0), 2)
	writeBars(t, root, KindBars, b)

	want := []string{
		"bars/date=2026-10-02/.bars-0915-001.parquet.tmp",
		"bars/date=2026-10-02/bars-0915-000.parquet",
		"bars/date=2026-10-02/bars-0915-002.parquet",
	}
	if got := listFiles(t, root); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	got, err := ReadBars(root, KindBars, BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{b, a})
}

func TestBackfillIsSeparateKind(t *testing.T) {
	root := t.TempDir()
	b := fixtureBar(1, time.Minute, at(9, 15, 0), 1)
	writeBars(t, root, KindBackfill, b)
	if got, _ := ReadBars(root, KindBars, BarQuery{}); len(got) != 0 {
		t.Fatalf("backfill read as tick-built bars: %v", got)
	}
	got, err := ReadBars(root, KindBackfill, BarQuery{})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, got, []domain.Bar{b})
}

func TestSchemaVersionIsChecked(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bars", "date=2026-10-02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "bars-0915-000.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	pw := parquet.NewGenericWriter[barRow](f,
		parquet.KeyValueMetadata(MetaSchema, BarSchemaName),
		parquet.KeyValueMetadata(MetaSchemaVersion, "99"),
	)
	if _, err := pw.Write([]barRow{barToRow(fixtureBar(1, time.Minute, at(9, 15, 0), 1))}); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := ReadBars(root, KindBars, BarQuery{}); err == nil {
		t.Fatal("read a file with the wrong schema version")
	}
}

func TestInvalidInputs(t *testing.T) {
	if _, err := NewBarWriter(t.TempDir(), KindTicks); err == nil {
		t.Error("NewBarWriter accepted the ticks kind")
	}
	if _, err := ReadBars(t.TempDir(), KindTicks, BarQuery{}); err == nil {
		t.Error("ReadBars accepted the ticks kind")
	}
	w, _ := NewBarWriter(t.TempDir(), KindBars)
	if err := w.Write(domain.Bar{Interval: 1500 * time.Millisecond}); err == nil {
		t.Error("wrote a fractional-second interval")
	}
	got, err := ReadBars(filepath.Join(t.TempDir(), "missing"), KindBars, BarQuery{})
	if err != nil || len(got) != 0 {
		t.Errorf("missing root = %v, %v; want empty", got, err)
	}
}

// writeTicks records ticks with the tick writer ingestion uses, so the reader
// is tested against the real on-disk contract.
func writeTicks(t *testing.T, root string, ticks ...domain.Tick) {
	t.Helper()
	w := NewTickWriter(root, 0)
	for _, tk := range ticks {
		if err := w.Write(tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func scanAll(t *testing.T, root string, q Query) []domain.Tick {
	t.Helper()
	var got []domain.Tick
	if err := ScanTicks(root, q, func(tk domain.Tick) error { got = append(got, tk); return nil }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestScanTicksReadsWriterFiles(t *testing.T) {
	root := t.TempDir()
	full := tickAt(408065, at(9, 15, 2).Add(1234567))
	full.LastTradeTime = at(9, 15, 0).UTC()
	full.OI = 5
	first := tickAt(1, at(9, 15, 2))
	first.LastTradeTime = time.Time{}
	second := tickAt(2, at(9, 15, 2))
	next := tickAt(1, at(9, 30, 0))
	// Recorded slightly out of exchange-time order, as a live feed can be, and
	// read back in recorded order.
	writeTicks(t, root, first, second, full, next)

	want := []domain.Tick{first, second, full, next}
	got := scanAll(t, root, Query{})
	if len(got) != len(want) {
		t.Fatalf("got %d ticks, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.ExchangeTime.Equal(w.ExchangeTime) || !g.LastTradeTime.Equal(w.LastTradeTime) || !g.ReceivedAt.Equal(w.ReceivedAt) {
			t.Errorf("tick %d times = %+v, want %+v", i, g, w)
		}
		g.ExchangeTime, g.LastTradeTime, g.ReceivedAt = time.Time{}, time.Time{}, time.Time{}
		w.ExchangeTime, w.LastTradeTime, w.ReceivedAt = time.Time{}, time.Time{}, time.Time{}
		if g != w {
			t.Errorf("tick %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[0].LastTradeTime.IsZero() {
		t.Errorf("unknown last trade time read back as %v", got[0].LastTradeTime)
	}

	got = scanAll(t, root, Query{From: at(9, 15, 1), To: at(9, 29, 0), Tokens: []uint32{2}})
	if len(got) != 1 || got[0].InstrumentToken != 2 {
		t.Fatalf("filtered = %+v; want only token 2", got)
	}

	stop := errors.New("stop")
	calls := 0
	err := ScanTicks(root, Query{}, func(domain.Tick) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("callback error not propagated: %v after %d calls", err, calls)
	}
}

func TestScanTicksFileOrder(t *testing.T) {
	root := t.TempDir()
	// Restarts inside one window add -N suffixes that do not sort by name
	// (-1 before the base file, -10 before -2), and a later window follows.
	var wantPrices []float64
	for run := range 12 {
		w := NewTickWriter(root, 0)
		price := float64(run + 1)
		wantPrices = append(wantPrices, price)
		tk := tickAt(1, at(9, 15, 2))
		tk.LastPrice = price
		if err := w.Write(tk); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	later := tickAt(1, at(9, 31, 0))
	later.LastPrice = 99
	writeTicks(t, root, later)
	wantPrices = append(wantPrices, 99)

	var gotPrices []float64
	for _, tk := range scanAll(t, root, Query{}) {
		gotPrices = append(gotPrices, tk.LastPrice)
	}
	if !reflect.DeepEqual(gotPrices, wantPrices) {
		t.Fatalf("replay order = %v, want %v (files %v)", gotPrices, wantPrices, listFiles(t, root))
	}
}

func TestScanTicksSkipsTempAndRejectsOtherSchemas(t *testing.T) {
	root := t.TempDir()
	writeTicks(t, root, tickAt(1, at(9, 15, 2)))
	dir := filepath.Join(root, "ticks", "date=2026-10-02")
	if err := os.WriteFile(filepath.Join(dir, ".ticks-091500-1.parquet.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scanAll(t, root, Query{}); len(got) != 1 {
		t.Fatalf("got %d ticks, want 1", len(got))
	}
	// A bar file under the tick kind is the wrong schema, not silently read.
	bars := filepath.Join(root, "bars", "date=2026-10-02")
	writeBars(t, root, KindBars, fixtureBar(1, time.Minute, at(9, 15, 0), 1))
	if err := os.Rename(filepath.Join(bars, "bars-0915-000.parquet"), filepath.Join(dir, "ticks-093000.parquet")); err != nil {
		t.Fatal(err)
	}
	if err := ScanTicks(root, Query{}, func(domain.Tick) error { return nil }); err == nil {
		t.Fatal("read a bar file as ticks")
	}
}

func TestReplayedTicksBuildTheBarsLiveBuilt(t *testing.T) {
	arrive := func(token uint32, exch time.Time, recv time.Time, price float64, vol int64) domain.Tick {
		tk := tickAt(token, recv)
		tk.ExchangeTime, tk.LastPrice, tk.Volume = exch.UTC(), price, vol
		return tk
	}
	recv := func(ts time.Time) time.Time { return ts.Add(100 * time.Millisecond) }
	// The 09:29:59 tick arrives after the 09:30:00 one, so it crosses into the
	// next file window and is late for the live Builder.
	arrived := []domain.Tick{
		arrive(1, at(9, 29, 0), recv(at(9, 29, 0)), 100, 1000),
		arrive(1, at(9, 30, 0), recv(at(9, 30, 0)), 101, 1100),
		arrive(1, at(9, 29, 59), recv(at(9, 30, 0)).Add(time.Millisecond), 90, 1150),
		arrive(1, at(9, 30, 30), recv(at(9, 30, 30)), 102, 1200),
	}
	build := func(ticks func(func(domain.Tick) error) error) ([]domain.Bar, bars.Stats) {
		t.Helper()
		b, err := bars.New(bars.Config{Derived: []time.Duration{}})
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.Bar
		if err := ticks(func(tk domain.Tick) error { out = append(out, b.Add(tk)...); return nil }); err != nil {
			t.Fatal(err)
		}
		return append(out, b.Advance(at(10, 0, 0))...), b.Stats()
	}

	live, liveStats := build(func(fn func(domain.Tick) error) error {
		for _, tk := range arrived {
			if err := fn(tk); err != nil {
				return err
			}
		}
		return nil
	})
	root := t.TempDir()
	writeTicks(t, root, arrived...)
	replayed, replayStats := build(func(fn func(domain.Tick) error) error { return ScanTicks(root, Query{}, fn) })

	if liveStats.Late != 1 || replayStats != liveStats {
		t.Fatalf("stats live %+v, replay %+v; want one late tick in both", liveStats, replayStats)
	}
	in := domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}
	equalBars(t, live, []domain.Bar{
		{Instrument: in, Interval: time.Minute, Start: at(9, 29, 0), End: at(9, 30, 0), Open: 100, High: 100, Low: 100, Close: 100},
		{Instrument: in, Interval: time.Minute, Start: at(9, 30, 0), End: at(9, 31, 0), Open: 101, High: 102, Low: 101, Close: 102, Volume: 200},
	})
	equalBars(t, replayed, live)
}

func TestScanReadsOnlyDatesThatCanMatch(t *testing.T) {
	root := t.TempDir()
	day := func(d, h, m, s int) time.Time { return time.Date(2026, 10, d, h, m, s, 0, ist) }
	late := tickAt(1, day(2, 0, 0, 1))
	late.ExchangeTime = day(1, 23, 59, 59).UTC()
	writeTicks(t, root, late)
	lateBar := fixtureBar(1, 30*time.Minute, day(1, 23, 30, 0), 1)
	writeBars(t, root, KindBars, lateBar)
	// A corrupt file on a later date proves a bounded scan never opens it.
	for kind, name := range map[string]string{KindTicks: "ticks-000000.parquet", KindBars: "bars-0000-000.parquet"} {
		dir := filepath.Join(root, kind, "date=2026-10-03")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{From: day(1, 0, 0, 0), To: day(2, 0, 0, 0)}
	got := scanAll(t, root, q)
	if len(got) != 1 || got[0].LastPrice != late.LastPrice {
		t.Fatalf("one-day ticks = %+v; want only the tick received after midnight", got)
	}
	bars, err := ReadBars(root, KindBars, BarQuery{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	equalBars(t, bars, []domain.Bar{lateBar})

	if err := ScanTicks(root, Query{}, func(domain.Tick) error { return nil }); err == nil {
		t.Error("an unbounded scan skipped the corrupt file")
	}
}
