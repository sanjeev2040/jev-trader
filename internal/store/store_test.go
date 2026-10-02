package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/domain"
)

func at(h, m, s int) time.Time { return time.Date(2026, 10, 1, h, m, s, 0, ist) }

func fixtureBar(token uint32, interval time.Duration, start time.Time, price float64) domain.Bar {
	return domain.Bar{
		InstrumentToken: token, Interval: interval, Start: start,
		Open: price, High: price + 1, Low: price - 1, Close: price + 0.5, Volume: 1234, Ticks: 7,
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
		g.Start, w.Start = time.Time{}, time.Time{}
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
		"bars/date=2026-10-01/bars-0915-000.parquet",
		"bars/date=2026-10-01/bars-0930-000.parquet",
		"bars/date=2026-10-02/bars-0915-000.parquet",
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
	dir := filepath.Join(root, "bars", "date=2026-10-01")
	if err := os.WriteFile(filepath.Join(dir, ".bars-0915-001.parquet.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := fixtureBar(1, time.Minute, at(9, 15, 0), 2)
	writeBars(t, root, KindBars, b)

	want := []string{
		"bars/date=2026-10-01/.bars-0915-001.parquet.tmp",
		"bars/date=2026-10-01/bars-0915-000.parquet",
		"bars/date=2026-10-01/bars-0915-002.parquet",
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
	w := &writer[barRow]{root: root, kind: KindBars, schema: "bars/v0"}
	if err := w.write(at(9, 15, 0), barToRow(fixtureBar(1, time.Minute, at(9, 15, 0), 1))); err != nil {
		t.Fatal(err)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
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

func writeTicks(t *testing.T, root string, ticks ...domain.Tick) {
	t.Helper()
	w := &writer[tickRow]{root: root, kind: KindTicks, schema: TickSchemaVersion}
	for _, tk := range ticks {
		if err := w.write(tk.ReceiveTime, tickToRow(tk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanTicks(t *testing.T) {
	root := t.TempDir()
	full := domain.Tick{
		InstrumentToken: 408065, ExchangeTime: at(9, 15, 1), LastTradeTime: at(9, 15, 0),
		ReceiveTime: at(9, 15, 2).Add(1234567), LastPrice: 1520.35, LastQuantity: 10,
		AveragePrice: 1519.8, VolumeTraded: 125000, TotalBuyQty: 4000, TotalSellQty: 3500, OI: 0,
	}
	first := domain.Tick{InstrumentToken: 1, ExchangeTime: at(9, 15, 2), ReceiveTime: at(9, 15, 2), LastPrice: 1}
	second := domain.Tick{InstrumentToken: 2, ExchangeTime: at(9, 15, 2), ReceiveTime: at(9, 15, 2), LastPrice: 2}
	next := domain.Tick{InstrumentToken: 1, ExchangeTime: at(9, 30, 0), ReceiveTime: at(9, 30, 0), LastPrice: 3}
	// Recorded slightly out of exchange-time order, as a live feed can be, and
	// read back in recorded order.
	writeTicks(t, root, first, second, full, next)

	var got []domain.Tick
	err := ScanTicks(root, Query{}, func(tk domain.Tick) error {
		got = append(got, tk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Tick{first, second, full, next}
	if len(got) != len(want) {
		t.Fatalf("got %d ticks, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.ExchangeTime.Equal(w.ExchangeTime) || !g.LastTradeTime.Equal(w.LastTradeTime) || !g.ReceiveTime.Equal(w.ReceiveTime) {
			t.Errorf("tick %d times = %+v, want %+v", i, g, w)
		}
		g.ExchangeTime, g.LastTradeTime, g.ReceiveTime = time.Time{}, time.Time{}, time.Time{}
		w.ExchangeTime, w.LastTradeTime, w.ReceiveTime = time.Time{}, time.Time{}, time.Time{}
		if g != w {
			t.Errorf("tick %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[0].LastTradeTime.IsZero() {
		t.Errorf("unknown last trade time read back as %v", got[0].LastTradeTime)
	}

	got = nil
	err = ScanTicks(root, Query{From: at(9, 15, 2), To: at(9, 30, 0), Tokens: []uint32{2}}, func(tk domain.Tick) error {
		got = append(got, tk)
		return nil
	})
	if err != nil || len(got) != 1 || got[0].InstrumentToken != 2 {
		t.Fatalf("filtered = %+v, %v; want only token 2", got, err)
	}

	stop := errors.New("stop")
	calls := 0
	err = ScanTicks(root, Query{}, func(domain.Tick) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("callback error not propagated: %v after %d calls", err, calls)
	}
}

func TestReplayedTicksBuildTheBarsLiveBuilt(t *testing.T) {
	recv := func(ts time.Time) time.Time { return ts.Add(100 * time.Millisecond) }
	// The 09:29:59 tick arrives after the 09:30:00 one, so it crosses into the
	// next file window and is late for the live Builder.
	arrived := []domain.Tick{
		{InstrumentToken: 1, ExchangeTime: at(9, 29, 0), ReceiveTime: recv(at(9, 29, 0)), LastPrice: 100, VolumeTraded: 1000},
		{InstrumentToken: 1, ExchangeTime: at(9, 30, 0), ReceiveTime: recv(at(9, 30, 0)), LastPrice: 101, VolumeTraded: 1100},
		{InstrumentToken: 1, ExchangeTime: at(9, 29, 59), ReceiveTime: recv(at(9, 30, 0)).Add(time.Millisecond), LastPrice: 90, VolumeTraded: 1150},
		{InstrumentToken: 1, ExchangeTime: at(9, 30, 30), ReceiveTime: recv(at(9, 30, 30)), LastPrice: 102, VolumeTraded: 1200},
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
	equalBars(t, live, []domain.Bar{
		{InstrumentToken: 1, Interval: time.Minute, Start: at(9, 29, 0), Open: 100, High: 100, Low: 100, Close: 100, Ticks: 1},
		{InstrumentToken: 1, Interval: time.Minute, Start: at(9, 30, 0), Open: 101, High: 102, Low: 101, Close: 102, Volume: 200, Ticks: 2},
	})
	equalBars(t, replayed, live)
}
