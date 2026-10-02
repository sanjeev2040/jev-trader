package marketdata

import (
	"context"
	"testing"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

func TestTicksForwardsTicksAndSkipsBars(t *testing.T) {
	in := make(chan Event, 4)
	in <- Event{Tick: &domain.Tick{InstrumentToken: 1}}
	in <- Event{Bar: &domain.Bar{Instrument: domain.Instrument{Token: 9}}}
	in <- Event{Tick: &domain.Tick{InstrumentToken: 2}}
	close(in)

	var got []uint32
	for tk := range Ticks(context.Background(), in, 1) {
		got = append(got, tk.InstrumentToken)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("got tokens %v, want [1 2]", got)
	}
}

func TestTicksStopsWhenCtxEndsWhileBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan Event, 2)
	in <- Event{Tick: &domain.Tick{InstrumentToken: 1}}
	in <- Event{Tick: &domain.Tick{InstrumentToken: 2}}
	out := Ticks(ctx, in, 0) // nobody reads, so the forwarder blocks
	cancel()
	for range out {
	}
}
