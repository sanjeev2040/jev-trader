package bars

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

var day = time.Date(2026, 10, 1, 9, 15, 0, 0, ist)

// at returns the session time hh:mm:ss on the fixture day.
func at(h, m, s int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, s, 0, ist)
}

func tick(token uint32, ts time.Time, price float64, cumVol int64) domain.Tick {
	return domain.Tick{InstrumentToken: token, ExchangeTime: ts, LastPrice: price, Volume: cumVol}
}

func bar(token uint32, interval time.Duration, start time.Time, o, h, l, c float64, vol int64) domain.Bar {
	return domain.Bar{Instrument: domain.Instrument{Token: token}, Interval: interval, Start: start, End: start.Add(interval), Open: o, High: h, Low: l, Close: c, Volume: vol}
}

func newBuilder(t *testing.T, cfg Config) *Builder {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// feed adds every tick and returns all bars closed along the way.
func feed(b *Builder, ticks ...domain.Tick) []domain.Bar {
	var out []domain.Bar
	for _, tk := range ticks {
		out = append(out, b.Add(tk)...)
	}
	return out
}

func assertBars(t *testing.T, got, want []domain.Bar) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d bars, want %d:\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) {
			t.Errorf("bar %d start = %v, want %v", i, got[i].Start, want[i].Start)
		}
		g, w := got[i], want[i]
		g.Start, w.Start = time.Time{}, time.Time{}
		if g != w {
			t.Errorf("bar %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

const m1, m5 = time.Minute, 5 * time.Minute

func TestOneMinuteBarClosesOnNextMinuteTick(t *testing.T) {
	b := newBuilder(t, Config{Derived: []time.Duration{}})
	got := feed(b,
		tick(1, at(9, 15, 0), 100, 1000), // baseline only: volume 0
		tick(1, at(9, 15, 10), 102, 1050),
		tick(1, at(9, 15, 30), 99, 1080),
		tick(1, at(9, 15, 59), 101, 1100),
	)
	if len(got) != 0 {
		t.Fatalf("bar closed early: %+v", got)
	}
	got = b.Add(tick(1, at(9, 16, 0), 103, 1110))
	assertBars(t, got, []domain.Bar{bar(1, m1, at(9, 15, 0), 100, 102, 99, 101, 100)})

	got = b.Advance(at(9, 30, 0))
	assertBars(t, got, []domain.Bar{bar(1, m1, at(9, 16, 0), 103, 103, 103, 103, 10)})
}

func TestFiveMinuteBarClosesWithItsLastMinute(t *testing.T) {
	b := newBuilder(t, Config{})
	var ticks []domain.Tick
	vol := int64(0)
	for m := 15; m <= 19; m++ {
		for _, s := range []int{0, 30} {
			vol += 10
			price := float64(100 + m - 15)
			if s == 30 {
				price += 0.5
			}
			ticks = append(ticks, tick(7, at(9, m, s), price, vol))
		}
	}
	got := feed(b, ticks...)
	// Four 1m bars close on the following minute's first tick.
	assertBars(t, got, []domain.Bar{
		bar(7, m1, at(9, 15, 0), 100, 100.5, 100, 100.5, 10),
		bar(7, m1, at(9, 16, 0), 101, 101.5, 101, 101.5, 20),
		bar(7, m1, at(9, 17, 0), 102, 102.5, 102, 102.5, 20),
		bar(7, m1, at(9, 18, 0), 103, 103.5, 103, 103.5, 20),
	})
	// The 09:19 bar's close completes the 09:15 5m bar in the same call.
	got = b.Add(tick(7, at(9, 20, 1), 105, vol+5))
	assertBars(t, got, []domain.Bar{
		bar(7, m1, at(9, 19, 0), 104, 104.5, 104, 104.5, 20),
		bar(7, m5, at(9, 15, 0), 100, 104.5, 100, 104.5, 90),
	})
}

func TestAdvanceWaitsForGrace(t *testing.T) {
	b := newBuilder(t, Config{Grace: 2 * time.Second, Derived: []time.Duration{}})
	b.Add(tick(1, at(9, 15, 5), 100, 0))
	if got := b.Advance(at(9, 16, 1)); len(got) != 0 {
		t.Fatalf("closed inside grace: %+v", got)
	}
	// A tick stamped before the boundary that arrives inside the grace still counts.
	b.Add(tick(1, at(9, 15, 59), 98, 0))
	got := b.Advance(at(9, 16, 2))
	assertBars(t, got, []domain.Bar{bar(1, m1, at(9, 15, 0), 100, 100, 98, 98, 0)})
}

func TestAdvanceClosesIlliquidDerivedBar(t *testing.T) {
	b := newBuilder(t, Config{})
	b.Add(tick(3, at(9, 16, 20), 50, 100))
	b.Add(tick(3, at(9, 16, 40), 51, 160))
	got := b.Advance(at(9, 17, 2))
	assertBars(t, got, []domain.Bar{bar(3, m1, at(9, 16, 0), 50, 51, 50, 51, 60)})
	if got := b.Advance(at(9, 20, 1)); len(got) != 0 {
		t.Fatalf("5m bar closed inside grace: %+v", got)
	}
	got = b.Advance(at(9, 20, 2))
	assertBars(t, got, []domain.Bar{bar(3, m5, at(9, 15, 0), 50, 51, 50, 51, 60)})
}

func TestLateTicksAreDroppedButVolumeIsKept(t *testing.T) {
	b := newBuilder(t, Config{Derived: []time.Duration{}})
	feed(b,
		tick(1, at(9, 15, 0), 100, 0),
		tick(1, at(9, 16, 0), 101, 100), // closes 09:15
		tick(1, at(9, 15, 59), 90, 150), // late: 09:15 already closed
		tick(1, at(9, 16, 30), 102, 200),
	)
	if got := b.Stats().Late; got != 1 {
		t.Fatalf("late = %d, want 1", got)
	}
	got := b.Advance(at(9, 18, 0))
	// The late tick's price is ignored; its volume shows up in the next accepted delta.
	assertBars(t, got, []domain.Bar{bar(1, m1, at(9, 16, 0), 101, 102, 101, 102, 200)})
}

func TestLateTickCannotReopenBarClosedByAdvance(t *testing.T) {
	b := newBuilder(t, Config{})
	b.Add(tick(1, at(9, 17, 10), 100, 0))
	first := b.Advance(at(9, 20, 5)) // closes 09:17 1m and the 09:15 5m bar
	assertBars(t, first, []domain.Bar{
		bar(1, m1, at(9, 17, 0), 100, 100, 100, 100, 0),
		bar(1, m5, at(9, 15, 0), 100, 100, 100, 100, 0),
	})
	// 09:19 belongs to the already-closed 5m window; it must not create a second one.
	if got := b.Add(tick(1, at(9, 19, 50), 99, 10)); len(got) != 0 {
		t.Fatalf("unexpected bars %+v", got)
	}
	if got := b.Stats().Late; got != 1 {
		t.Fatalf("late = %d, want 1", got)
	}
	if got := b.Advance(at(10, 0, 0)); len(got) != 0 {
		t.Fatalf("late tick produced bars: %+v", got)
	}
}

func TestTickOlderThanOpenBarIsLate(t *testing.T) {
	b := newBuilder(t, Config{Derived: []time.Duration{}})
	b.Add(tick(1, at(9, 18, 0), 100, 0))
	b.Add(tick(1, at(9, 17, 30), 99, 0))
	if got := b.Stats().Late; got != 1 {
		t.Fatalf("late = %d, want 1", got)
	}
}

func TestVolume(t *testing.T) {
	prev := day.AddDate(0, 0, -1)
	cases := []struct {
		name  string
		ticks []domain.Tick
		want  int64
	}{
		{"first tick is baseline", []domain.Tick{tick(1, at(9, 15, 1), 10, 5000)}, 0},
		{"deltas of cumulative volume", []domain.Tick{
			tick(1, at(9, 15, 1), 10, 5000), tick(1, at(9, 15, 2), 10, 5040), tick(1, at(9, 15, 3), 10, 5100),
		}, 100},
		{"stale lower value on same day is ignored", []domain.Tick{
			tick(1, at(9, 15, 1), 10, 5000), tick(1, at(9, 15, 2), 10, 4900), tick(1, at(9, 15, 3), 10, 5030),
		}, 30},
		{"reset on a new day counts the whole value", []domain.Tick{
			tick(1, prev.Add(6*time.Hour), 10, 900000), tick(1, at(9, 15, 1), 10, 1200),
		}, 1200},
		{"higher value on a new day counts the whole value", []domain.Tick{
			tick(1, prev.Add(6*time.Hour), 10, 400000), tick(1, at(9, 15, 1), 10, 450000),
		}, 450000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder(t, Config{Derived: []time.Duration{}})
			feed(b, tc.ticks...)
			got := b.Advance(at(9, 30, 0))
			last := got[len(got)-1]
			if last.Volume != tc.want {
				t.Fatalf("volume = %d, want %d", last.Volume, tc.want)
			}
		})
	}
}

func TestInvalidTicks(t *testing.T) {
	b := newBuilder(t, Config{})
	feed(b,
		domain.Tick{InstrumentToken: 1, LastPrice: 100},
		tick(1, at(9, 15, 0), 0, 0),
		tick(1, at(9, 15, 0), -1, 0),
	)
	if got := b.Stats().Invalid; got != 3 {
		t.Fatalf("invalid = %d, want 3", got)
	}
	if got := b.Advance(at(10, 0, 0)); len(got) != 0 {
		t.Fatalf("invalid ticks produced bars: %+v", got)
	}
}

func TestGapClosesDerivedBarOnLaterTick(t *testing.T) {
	b := newBuilder(t, Config{})
	got := feed(b,
		tick(1, at(9, 16, 0), 100, 0),
		tick(1, at(9, 22, 0), 105, 50),
	)
	// No bars are invented for the empty minutes.
	assertBars(t, got, []domain.Bar{
		bar(1, m1, at(9, 16, 0), 100, 100, 100, 100, 0),
		bar(1, m5, at(9, 15, 0), 100, 100, 100, 100, 0),
	})
}

func TestOutputOrderIsDeterministic(t *testing.T) {
	for range 20 {
		b := newBuilder(t, Config{})
		for _, token := range []uint32{30, 10, 20} {
			b.Add(tick(token, at(9, 19, 0), float64(token), 0))
		}
		got := b.Advance(at(9, 21, 0))
		var keys []string
		for _, x := range got {
			keys = append(keys, fmt.Sprintf("%v/%d", x.Interval, x.Instrument.Token))
		}
		want := []string{"1m0s/10", "1m0s/20", "1m0s/30", "5m0s/10", "5m0s/20", "5m0s/30"}
		if !slices.Equal(keys, want) {
			t.Fatalf("order = %v, want %v", keys, want)
		}
	}
}

func TestAggregateMatchesBuilder(t *testing.T) {
	b := newBuilder(t, Config{})
	var all []domain.Bar
	vol := int64(0)
	for i := range 23 * 60 / 10 { // ticks every 10s from 09:15 to 09:38
		vol += int64(i % 7)
		price := 100 + float64(i%13) - float64(i%5)
		all = append(all, b.Add(tick(1, day.Add(time.Duration(i)*10*time.Second), price, vol))...)
	}
	all = append(all, b.Advance(at(23, 0, 0))...)
	var ones, fives []domain.Bar
	for _, x := range all {
		if x.Interval == m1 {
			ones = append(ones, x)
		} else {
			fives = append(fives, x)
		}
	}
	slices.Reverse(ones) // Aggregate must not depend on input order
	got, err := Aggregate(ones, m5)
	if err != nil {
		t.Fatal(err)
	}
	assertBars(t, got, fives)
}

func TestAggregateErrors(t *testing.T) {
	one := bar(1, m1, at(9, 15, 0), 1, 1, 1, 1, 0)
	five := bar(1, m5, at(9, 15, 0), 1, 1, 1, 1, 0)
	if _, err := Aggregate([]domain.Bar{one, five}, 15*time.Minute); err == nil {
		t.Error("mixed intervals accepted")
	}
	if _, err := Aggregate([]domain.Bar{five}, 3*time.Minute); err == nil {
		t.Error("non-dividing interval accepted")
	}
	if _, err := Aggregate([]domain.Bar{one}, time.Hour); err == nil {
		t.Error("interval not aligned to IST accepted")
	}
	if got, err := Aggregate(nil, m5); err != nil || got != nil {
		t.Errorf("Aggregate(nil) = %v, %v", got, err)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Base: 7 * time.Minute},
		{Base: time.Hour},
		{Derived: []time.Duration{90 * time.Second}},
		{Derived: []time.Duration{time.Minute}},
		{Derived: []time.Duration{m5, m5}},
		{Grace: -time.Second},
	}
	for _, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
	if _, err := New(Config{Derived: []time.Duration{m5, 15 * time.Minute}}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
