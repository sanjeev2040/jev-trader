// Package replay is the backtest driver (issue #10). It runs stored market
// data through the same engine live trading uses, once per decider, each in
// its own decision-log run with a fresh paper broker and a simulated clock,
// and then compares the runs in an evaluation report.
package replay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/clock"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

// DefaultLookahead are the horizons the comparison checks calls at.
var DefaultLookahead = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

// defaultBuyHoldNotional is the benchmark's spend per instrument and day.
const defaultBuyHoldNotional = 20_000

// Spec is one decider to replay the data through.
type Spec struct {
	// Name labels the run: jev, recorded, hold, momentum, ...
	Name    string
	Decider decide.Decider
	// Notes is called after the run and may return remarks for the report,
	// such as how many recorded answers a regression replay could not find.
	Notes func() []string
}

// Config configures a replay.
type Config struct {
	Log *decisionlog.Log
	// NewSource returns a fresh source over the stored data. It is called
	// once per decider, so every decider sees the same events.
	NewSource func() (marketdata.Source, error)
	// SourceName and Label say what the data is (ticks, bars, bars_backfill).
	SourceName string
	Label      string

	Limits   risk.Limits
	Paper    paper.Config
	Bars     bars.Config
	Snapshot snapshot.Config
	// IndexToken is the NIFTY 50 instrument; zero means none.
	IndexToken uint32
	// Window limits trading to a time window of each IST day. Earlier bars of
	// the day still warm the snapshots up; the rest of the day only provides
	// the prices the comparison looks at. The zero value is the whole day.
	Window engine.Window

	// RunPrefix starts every run id: <prefix>-<spec name>.
	RunPrefix string
	// Latency, when positive, replaces every decision's latency so a result
	// does not depend on how fast the decider answered. Zero applies the
	// decision's own latency.
	Latency time.Duration
	// DecideTimeout is the per-call decider deadline; zero means the engine's
	// default.
	DecideTimeout time.Duration
	// Horizon is how many bars ahead the report scores a BUY or SELL; zero
	// means DefaultHorizon.
	Horizon int

	// Walkthrough adds each run's chronological walk to the report.
	Walkthrough WalkMode
	// Compare adds the comparison with the real market that followed.
	Compare bool
	// Lookahead are the horizons Compare checks each call at; empty means
	// DefaultLookahead.
	Lookahead []time.Duration
	// BuyHoldNotional is what Compare's buy-and-hold benchmark spends per
	// instrument and day; zero means 20,000.
	BuyHoldNotional float64

	Logger *slog.Logger
}

// Run replays the data through each spec in turn and returns the report.
func Run(ctx context.Context, cfg Config, specs []Spec) (*Report, error) {
	switch {
	case cfg.Log == nil, cfg.NewSource == nil:
		return nil, errors.New("replay: a decision log and a source are required")
	case len(specs) == 0:
		return nil, errors.New("replay: no deciders to replay")
	case cfg.RunPrefix == "":
		return nil, errors.New("replay: run prefix is required")
	}
	gate, err := risk.NewGate(cfg.Limits)
	if err != nil {
		return nil, err
	}
	base := cfg.Bars.Base
	if base == 0 {
		base = time.Minute
	}

	var refs []RunRef
	var notes []string
	var prices []domain.Bar
	for i, spec := range specs {
		runID := cfg.RunPrefix + "-" + spec.Name
		src, err := cfg.NewSource()
		if err != nil {
			return nil, err
		}
		sim := clock.NewSim(time.Time{})
		broker, err := paper.New(sim, cfg.Paper)
		if err != nil {
			return nil, err
		}
		var collected []domain.Bar
		e, err := engine.New(engine.Config{
			RunID: runID, Mode: "replay", DeciderName: spec.Name, SourceName: cfg.SourceName,
			Source: src, Clock: sim, Decider: spec.Decider, Broker: broker, Gate: gate, Log: cfg.Log,
			Bars: cfg.Bars, Snapshot: cfg.Snapshot, IndexToken: cfg.IndexToken, Window: cfg.Window,
			WaitForDecisions: true, Latency: cfg.Latency, DecideTimeout: cfg.DecideTimeout,
			BarSink: func(b domain.Bar) {
				if b.Interval == base {
					collected = append(collected, b)
				}
			},
			Logger: cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
		res, err := e.Run(ctx)
		if err != nil {
			return nil, fmt.Errorf("replay %s: %w", runID, err)
		}
		if i == 0 {
			prices = collected // every run sees the same data
		}
		refs = append(refs, RunRef{ID: runID, Decider: spec.Name})
		if res.OpenOrders > 0 || len(res.Positions) > 0 {
			notes = append(notes, fmt.Sprintf("%s ended with %d open order(s) and %d open position(s), marked at the last price.", runID, res.OpenOrders, len(res.Positions)))
		}
		if res.SnapshotErrors > 0 {
			notes = append(notes, fmt.Sprintf("%s skipped %d bar(s) whose snapshot could not be built.", runID, res.SnapshotErrors))
		}
		if spec.Notes != nil {
			notes = append(notes, spec.Notes()...)
		}
	}

	rep, err := Evaluate(ctx, cfg.Log, refs, prices, base, cfg.Horizon)
	if err != nil {
		return nil, err
	}
	rep.Label = cfg.Label
	rep.Notes = notes
	win := dayWindow{Start: cfg.Window.Start, End: cfg.Window.End}
	rep.Window = win.String()
	if cfg.Walkthrough != WalkOff || cfg.Compare {
		tape := newForward(prices)
		if cfg.Walkthrough != WalkOff {
			for _, ref := range refs {
				w, err := BuildWalkthrough(ctx, cfg.Log, ref, cfg.Walkthrough, tape, win)
				if err != nil {
					return nil, err
				}
				rep.Walkthrough = append(rep.Walkthrough, w)
			}
		}
		if cfg.Compare {
			lookahead := cfg.Lookahead
			if len(lookahead) == 0 {
				lookahead = DefaultLookahead
			}
			notional := cfg.BuyHoldNotional
			if notional <= 0 {
				notional = defaultBuyHoldNotional
			}
			rep.Comparison, err = BuildComparison(ctx, cfg.Log, refs, rep.Runs, tape, CompareConfig{
				Lookahead: lookahead, Window: win, IndexToken: cfg.IndexToken,
				Notional: notional, Costs: cfg.Paper.Costs,
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return rep, nil
}
