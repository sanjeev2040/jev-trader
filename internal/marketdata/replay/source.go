// Package replay is the marketdata.Source that reads stored data: ticks
// recorded by `jevtrader record`, bars built from them, or bars backfilled
// over Kite's historical REST API. It is the replay mode's edge; everything
// downstream of it is the same code live trading runs.
package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

// Kind says which stored data a Source reads.
type Kind string

// Kinds of stored data.
const (
	// Ticks replays recorded ticks in the order they were recorded, so the
	// bar builder sees exactly what the live run saw.
	Ticks Kind = store.KindTicks
	// Bars replays bars built from recorded ticks.
	Bars Kind = store.KindBars
	// Backfill replays bars fetched from Kite historical candles. They can
	// differ from tick-built bars, so results on them are labelled as such
	// and are not comparable with tick-built runs.
	Backfill Kind = store.KindBackfill
)

// Config configures a Source.
type Config struct {
	// Root is the data root that `record` and `backfill` write under.
	Root string
	Kind Kind
	// From and To bound the data by event time, [From, To). Zero means no bound.
	From, To time.Time
	// Tokens limits the instruments; empty means all.
	Tokens []uint32
	// BarInterval selects the bars of one interval when Kind is Bars or
	// Backfill. Zero means one minute.
	BarInterval time.Duration
}

// Source replays stored market data.
type Source struct {
	cfg Config
}

var _ marketdata.Source = (*Source)(nil)

// New returns a Source for cfg.
func New(cfg Config) (*Source, error) {
	switch cfg.Kind {
	case Ticks, Bars, Backfill:
	default:
		return nil, fmt.Errorf("replay: unknown data kind %q", cfg.Kind)
	}
	if cfg.Root == "" {
		return nil, errors.New("replay: data root is required")
	}
	if !cfg.From.IsZero() && !cfg.To.IsZero() && !cfg.To.After(cfg.From) {
		return nil, fmt.Errorf("replay: to %s is not after from %s", cfg.To, cfg.From)
	}
	if cfg.BarInterval == 0 {
		cfg.BarInterval = time.Minute
	}
	return &Source{cfg: cfg}, nil
}

// Run publishes the stored events on out in recorded order and returns nil
// once the data is exhausted. It blocks on a full out channel, never dropping
// an event, and returns ctx's error if ctx ends first.
func (s *Source) Run(ctx context.Context, out chan<- marketdata.Event) error {
	q := store.Query{From: s.cfg.From, To: s.cfg.To, Tokens: s.cfg.Tokens}
	send := func(ev marketdata.Event) error {
		select {
		case out <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.cfg.Kind == Ticks {
		return store.ScanTicks(s.cfg.Root, q, func(t domain.Tick) error {
			return send(marketdata.Event{Tick: &t})
		})
	}
	return store.ScanBars(s.cfg.Root, string(s.cfg.Kind), store.BarQuery{Query: q, Interval: s.cfg.BarInterval}, func(b domain.Bar) error {
		return send(marketdata.Event{Bar: &b})
	})
}

// Label describes the data for reports.
func (c Config) Label() string {
	switch c.Kind {
	case Backfill:
		return "bars_backfill (Kite historical candles; not comparable with tick-built runs)"
	case Bars:
		return "bars (built from recorded ticks)"
	}
	return "ticks (recorded)"
}
