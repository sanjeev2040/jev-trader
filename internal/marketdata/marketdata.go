// Package marketdata defines the market-data edge shared by live trading,
// paper trading and replay.
//
// A Source publishes Events on a channel owned by the caller. The live Kite
// ticker lives in marketdata/kite; nothing outside that package imports the
// vendor ticker.
package marketdata

import (
	"context"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Event is one market-data update. Exactly one of Tick and Bar is set.
type Event struct {
	Tick *domain.Tick
	Bar  *domain.Bar
}

// Source produces market data.
type Source interface {
	// Run publishes events on out until ctx ends or the data is exhausted.
	// It closes nothing; the caller owns out.
	Run(ctx context.Context, out chan<- Event) error
}

// Ticks returns a channel carrying the tick of every tick event read from
// in, for feeding the tick recorder. Bar events are skipped. Every event
// already sent on in is forwarded; the returned channel is closed once in is
// closed and drained, or when ctx ends.
func Ticks(ctx context.Context, in <-chan Event, buffer int) <-chan domain.Tick {
	out := make(chan domain.Tick, buffer)
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Tick == nil {
				continue
			}
			select {
			case out <- *ev.Tick:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
