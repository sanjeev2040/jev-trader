package replay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func at(h, m, s int) time.Time { return time.Date(2026, 10, 5, h, m, s, 0, ist).UTC() }

func writeTicks(t *testing.T, root string, ticks ...domain.Tick) {
	t.Helper()
	w := store.NewTickWriter(root, 0)
	for _, tk := range ticks {
		if err := w.Write(tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func tick(token uint32, sym string, ts time.Time, price float64) domain.Tick {
	return domain.Tick{InstrumentToken: token, Symbol: sym, ExchangeTime: ts, ReceivedAt: ts.Add(50 * time.Millisecond), LastPrice: price, Volume: 100}
}

func drain(t *testing.T, s *Source) []marketdata.Event {
	t.Helper()
	out := make(chan marketdata.Event, 64)
	if err := s.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var evs []marketdata.Event
	for ev := range out {
		evs = append(evs, ev)
	}
	return evs
}

func TestReplaysTicksInRecordedOrder(t *testing.T) {
	root := t.TempDir()
	writeTicks(t, root,
		tick(1, "NSE:INFY", at(9, 15, 1), 1500),
		tick(2, "NSE:TCS", at(9, 15, 2), 3000),
		tick(1, "NSE:INFY", at(9, 15, 3), 1501),
		tick(1, "NSE:INFY", at(9, 16, 0), 1502),
	)
	s, err := New(Config{Root: root, Kind: Ticks, To: at(9, 16, 0)})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, s)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 (the To bound is exclusive)", len(evs))
	}
	for i, want := range []float64{1500, 3000, 1501} {
		if evs[i].Tick == nil || evs[i].Bar != nil || evs[i].Tick.LastPrice != want {
			t.Errorf("event %d = %+v, want a tick at %v", i, evs[i], want)
		}
	}

	only, _ := New(Config{Root: root, Kind: Ticks, Tokens: []uint32{2}})
	if evs := drain(t, only); len(evs) != 1 || evs[0].Tick.Symbol != "NSE:TCS" {
		t.Errorf("token filter: %+v", evs)
	}
	none, _ := New(Config{Root: t.TempDir(), Kind: Ticks})
	if evs := drain(t, none); len(evs) != 0 {
		t.Errorf("empty store produced %d events", len(evs))
	}
}

func TestReplaysBarsOfOneKind(t *testing.T) {
	root := t.TempDir()
	inst := domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}
	mk := func(start time.Time, iv time.Duration, c float64) domain.Bar {
		return domain.Bar{Instrument: inst, Interval: iv, Start: start, End: start.Add(iv), Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	for _, kind := range []string{store.KindBars, store.KindBackfill} {
		w, err := store.NewBarWriter(root, kind)
		if err != nil {
			t.Fatal(err)
		}
		c := 100.0
		if kind == store.KindBackfill {
			c = 200
		}
		if err := w.Write(mk(at(9, 15, 0), time.Minute, c), mk(at(9, 16, 0), time.Minute, c+1), mk(at(9, 15, 0), 5*time.Minute, c)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for kind, want := range map[Kind]float64{Bars: 100, Backfill: 200} {
		s, err := New(Config{Root: root, Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		evs := drain(t, s)
		if len(evs) != 2 {
			t.Fatalf("%s: %d events, want the two 1m bars", kind, len(evs))
		}
		if evs[0].Bar == nil || evs[0].Tick != nil || evs[0].Bar.Close != want || evs[1].Bar.Close != want+1 {
			t.Errorf("%s: events %+v %+v", kind, evs[0].Bar, evs[1].Bar)
		}
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	root := t.TempDir()
	writeTicks(t, root, tick(1, "NSE:INFY", at(9, 15, 1), 1500), tick(1, "NSE:INFY", at(9, 15, 2), 1501))
	s, _ := New(Config{Root: root, Kind: Ticks})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx, make(chan marketdata.Event)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNewValidates(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no kind":    {Root: "x"},
		"no root":    {Kind: Ticks},
		"empty span": {Root: "x", Kind: Ticks, From: at(10, 0, 0), To: at(9, 0, 0)},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if l := (Config{Kind: Backfill}).Label(); l == (Config{Kind: Ticks}).Label() {
		t.Error("labels must tell the kinds apart")
	}
}
