// Package engine is the single event loop that run and replay share
// (issue #10). It composes the bar builder, snapshot, decider, risk gate,
// order manager and decision log. Only the edges differ between modes: the
// market-data source, the clock, the decider and the broker. One flag,
// WaitForDecisions, is the only behavioural difference between the paths.
//
// One goroutine, the one that calls Run, owns all mutable trading state: the
// bar builder, positions, open orders and risk counters. Deciders run in
// their own goroutines and send their answers back on a channel.
//
// For every closed bar of a watched instrument the engine builds a snapshot,
// asks the decider, has the risk gate judge the answer, writes the cycle to
// the decision log, and only then submits an approved order. Risk-driven exits
// (square-off, loss limit, kill switch, per-position stop and time exit) do
// not wait for the decider and are logged as cycles of their own.
package engine

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
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/orders"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

// Defaults.
const (
	DefaultDecideTimeout = 800 * time.Millisecond
	DefaultMaxInFlight   = 8
	DefaultEventBuffer   = 8192
	// DefaultDecideEvery is how often the trading commands ask the decider
	// about an instrument: every fifth minute, not every bar (issue #53).
	DefaultDecideEvery = 5 * time.Minute
	// sessionOpen is the NSE session open in IST. Bars that start before it
	// (the pre-open session) are not decision bars.
	sessionOpenHour, sessionOpenMinute = 9, 15
)

var _ orders.Journal = (*decisionlog.Journal)(nil)

// Feed is implemented by brokers that fill orders against the market-data
// stream, such as the paper broker. The engine offers it every tick and bar.
type Feed interface {
	OnTick(domain.Tick)
	OnBar(domain.Bar)
}

// Window limits trading to part of each IST day, as offsets from IST midnight
// (09:15 is 9*time.Hour + 15*time.Minute). The market data before Start still
// flows through the engine, so snapshots, indicators and marks warm up, but
// no decision is asked for on a bar that starts before Start. Nothing at or
// after End reaches the clock, the broker or the risk book; such data goes
// only to the bar sink, so evaluation can still see the rest of the day. A
// zero Start or End leaves that side open.
type Window struct {
	Start, End time.Duration
}

// Config assembles an engine. Source, Clock, Decider, Broker, Gate and Log are
// the swappable edges and shared parts; the rest tunes them.
type Config struct {
	// RunID names the run in the decision log; cycle ids are derived from it.
	RunID string
	// Mode is paper, live or replay. It is recorded, not interpreted.
	Mode string
	// DeciderName and SourceName describe the run in the decision log.
	DeciderName string
	SourceName  string
	Note        string

	Source  marketdata.Source
	Clock   clock.Clock
	Decider decide.Decider
	Broker  orders.Broker
	Gate    *risk.Gate
	Log     *decisionlog.Log

	// Book is the risk book; nil means an empty one.
	Book *risk.Book
	// Foreign are holdings and positions the bot did not open. The engine
	// registers them with the book, which keeps them out of the capital
	// budget and never lets an order, exit or rotation touch them.
	Foreign []domain.Position
	// Bars configures the bar builder. Its Base interval is the decision
	// interval; it defaults to one minute.
	Bars bars.Config
	// DecideEvery is how often an instrument is put to the decider: only the
	// base bars that close on a multiple of it start a cycle (09:20, 09:25, ...
	// for five minutes). The other bars still feed the snapshot's history, the
	// marks and the risk exits, which never wait for a decision. It must be a
	// multiple of the base interval that divides 30 minutes, so it lines up
	// with the exchange's day. Zero decides on every base bar.
	DecideEvery time.Duration
	// Snapshot configures the state sent to the decider.
	Snapshot snapshot.Config
	// IndexToken is the NIFTY 50 instrument. Its bars feed the snapshot's
	// index context and are never traded. Zero means no index.
	IndexToken uint32
	// Window limits trading to part of each day; the zero value is the whole
	// day.
	Window Window

	// WaitForDecisions makes the engine stop reading the source until every
	// in-flight decision has resolved, so the simulated clock cannot run ahead
	// of the decision. Replay sets it; live trading does not.
	WaitForDecisions bool
	// DecideTimeout is the per-call decider deadline. Zero means
	// DefaultDecideTimeout.
	DecideTimeout time.Duration
	// MaxInFlight caps concurrent decider calls. Zero means DefaultMaxInFlight.
	MaxInFlight int
	// Latency, when positive, replaces every decision's latency when the
	// answer is applied. Replay uses it so the result does not depend on how
	// fast the decider answered on the day. Zero applies the decision's own
	// latency: measured for Jev, recorded for the recorded-answer decider.
	Latency time.Duration
	// AdvanceEvery makes the engine close due bars on a timer as well as on
	// events, so illiquid instruments close on time. Zero disables it; live
	// trading sets it, replay's simulated clock moves with the data.
	AdvanceEvery time.Duration
	// EventBuffer is the capacity of the source's channel. Zero means
	// DefaultEventBuffer.
	EventBuffer int

	// Kill delivers operator and watchdog kill requests (risk.WatchKill).
	Kill <-chan risk.Reason
	// Recorded are the intents logged before a restart. The order manager
	// reconciles them against the broker's order book before the first order.
	Recorded []domain.OrderIntent

	// TickSink receives every tick, for the tick recorder. It runs on the
	// engine loop, so it must not block.
	TickSink func(domain.Tick)
	// BarSink receives every closed bar of every interval, for the bar store
	// and for evaluation. It runs on the engine loop.
	BarSink func(domain.Bar)

	Logger *slog.Logger
}

// Stats counts what the engine did.
type Stats struct {
	Ticks, BarEvents  int // market-data events consumed
	Bars              int // closed base-interval bars of watched instruments
	OffCadence        int // of those, bars not on the decision cadence, so not decided on
	Cycles            int // decision cycles logged
	Holds             int // cycles whose answer was a HOLD
	HoldSubstitutions int // cycles whose HOLD replaced a decider failure
	Approved          int // decisions the risk gate approved
	Exits             int // risk-driven exit orders
	Rotations         int // sales of the bot's own positions to free capital for a stronger entry
	OrdersPlaced      int
	PlaceFailures     int
	Fills             int
	BusySkips         int // bars skipped because the instrument had a decision in flight
	SnapshotErrors    int // bars whose snapshot could not be built
	UnknownUpdates    int // broker updates for orders the manager does not know
}

// Result is what a run leaves behind.
type Result struct {
	Stats
	Positions      []domain.Position // open at the end
	PnL            float64           // cumulative after costs, with open positions marked
	Realized       float64           // cumulative realized after costs
	OpenOrders     int
	Reconciliation orders.Reconciliation
}

// Engine runs one trading or replay session. Use New, then Run once.
type Engine struct {
	cfg      Config
	log      *slog.Logger
	clk      clock.Clock
	settable clock.Settable // clk, if the engine drives it
	decider  decide.Decider
	gate     *risk.Gate
	book     *risk.Book
	mgr      *orders.Manager
	feed     Feed
	updates  <-chan domain.OrderUpdate
	builder  *bars.Builder
	aliases  *snapshot.Aliases
	base     time.Duration

	events  chan marketdata.Event
	results chan result
	sem     chan struct{}

	history   map[uint32][]domain.Bar // closed decision bars of the current session day
	pending   map[uint32]bool         // instruments with a decision in flight
	rotations map[uint32]*rotation    // entries waiting for the sale of the keyed instrument to fill
	flight    int
	seq       int

	stats Stats

	// Equity accounting across trading days: the risk book resets its PnL
	// each day, the log carries a running total.
	pnlDay                  string
	dayPnL, dayRealized     float64
	carryPnL, carryRealized float64
	lastEquity              time.Time
}

// New validates cfg and returns an engine.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.RunID == "":
		return nil, errors.New("engine: run id is required")
	case cfg.Source == nil, cfg.Clock == nil, cfg.Decider == nil, cfg.Broker == nil, cfg.Gate == nil, cfg.Log == nil:
		return nil, errors.New("engine: source, clock, decider, broker, gate and log are required")
	case cfg.DecideTimeout < 0, cfg.MaxInFlight < 0, cfg.Latency < 0, cfg.AdvanceEvery < 0, cfg.EventBuffer < 0:
		return nil, errors.New("engine: negative setting")
	case cfg.DecideEvery < 0:
		return nil, errors.New("engine: negative decision cadence")
	case cfg.Window.Start < 0, cfg.Window.End < 0, cfg.Window.End != 0 && cfg.Window.End <= cfg.Window.Start:
		return nil, errors.New("engine: window end must be after its start")
	}
	if err := cfg.Snapshot.Validate(); err != nil {
		return nil, err
	}
	if cfg.DecideTimeout == 0 {
		cfg.DecideTimeout = DefaultDecideTimeout
	}
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = DefaultMaxInFlight
	}
	if cfg.EventBuffer == 0 {
		cfg.EventBuffer = DefaultEventBuffer
	}
	if cfg.Book == nil {
		cfg.Book = risk.NewBook()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	builder, err := bars.New(cfg.Bars)
	if err != nil {
		return nil, err
	}
	mgr, err := orders.NewManager(cfg.Broker, cfg.Log.Journal(cfg.RunID))
	if err != nil {
		return nil, err
	}
	base := cfg.Bars.Base
	if base == 0 {
		base = time.Minute
	}
	if cfg.DecideEvery > 0 && (cfg.DecideEvery%base != 0 || (30*time.Minute)%cfg.DecideEvery != 0) {
		return nil, fmt.Errorf("engine: decision cadence %s must be a multiple of the %s base interval and divide 30m", cfg.DecideEvery, base)
	}
	e := &Engine{
		cfg:       cfg,
		log:       cfg.Logger.With("run", cfg.RunID),
		clk:       cfg.Clock,
		gate:      cfg.Gate,
		book:      cfg.Book,
		mgr:       mgr,
		updates:   cfg.Broker.Updates(),
		builder:   builder,
		aliases:   snapshot.NewAliases(),
		base:      base,
		results:   make(chan result, cfg.MaxInFlight),
		sem:       make(chan struct{}, cfg.MaxInFlight),
		history:   make(map[uint32][]domain.Bar),
		pending:   make(map[uint32]bool),
		rotations: make(map[uint32]*rotation),
	}
	for _, p := range cfg.Foreign {
		cfg.Book.SetForeign(p)
	}
	e.settable, _ = cfg.Clock.(clock.Settable)
	e.feed, _ = cfg.Broker.(Feed)
	// Every failure of the decider becomes a HOLD, whatever the caller passed.
	e.decider = cfg.Decider
	if _, ok := cfg.Decider.(*decide.HoldOnFailure); !ok {
		e.decider = decide.NewHoldOnFailure(cfg.Decider, decide.WithNow(cfg.Clock.Now))
	}
	return e, nil
}

// Run reconciles the order book, then consumes the source until it is
// exhausted or ctx ends, and returns what the run did. It returns ctx's error
// when ctx ends first, and the source's error if the source fails. A failure
// to write the decision log stops the run: nothing may be ordered that is not
// logged.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	rec, err := e.mgr.Reconcile(ctx, e.cfg.Recorded)
	if err != nil {
		return Result{}, fmt.Errorf("engine: reconcile: %w", err)
	}
	if len(rec.Adopted)+len(rec.NotPlaced)+len(rec.Unrecorded) > 0 {
		e.log.Warn("reconciled recorded orders", "adopted", len(rec.Adopted), "not_placed", len(rec.NotPlaced), "unrecorded", len(rec.Unrecorded))
	}
	if err := e.cfg.Log.StartRun(ctx, decisionlog.Run{
		ID: e.cfg.RunID, Mode: e.cfg.Mode, Decider: e.cfg.DeciderName, Source: e.cfg.SourceName,
		Note: e.cfg.Note, StartedAt: e.clk.Now(),
	}); err != nil {
		return Result{}, err
	}

	srcCtx, cancel := context.WithCancel(ctx)
	e.events = make(chan marketdata.Event, e.cfg.EventBuffer)
	srcDone := make(chan error, 1)
	go func() {
		err := e.cfg.Source.Run(srcCtx, e.events)
		close(e.events) // the source closes nothing; the engine owns the channel
		srcDone <- err
	}()

	loopErr := e.loop(ctx)
	cancel()
	for range e.events { // let a source blocked on a full channel finish
	}
	srcErr := <-srcDone

	res := e.result(rec)
	switch {
	case loopErr != nil:
		return res, loopErr
	case srcErr != nil && !errors.Is(srcErr, context.Canceled):
		return res, fmt.Errorf("engine: market data: %w", srcErr)
	}
	return res, ctx.Err()
}

func (e *Engine) loop(ctx context.Context) error {
	var advance <-chan time.Time
	if e.cfg.AdvanceEvery > 0 {
		advance = e.clk.After(e.cfg.AdvanceEvery)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-e.events:
			if !ok {
				// The data is exhausted: let in-flight decisions finish.
				if err := e.await(ctx); err != nil {
					return err
				}
				return e.drain(ctx)
			}
			if err := e.onEvent(ctx, ev); err != nil {
				return err
			}
		case r := <-e.results:
			if err := e.onResult(ctx, r); err != nil {
				return err
			}
		case u := <-e.updates:
			if err := e.onUpdate(ctx, u); err != nil {
				return err
			}
		case reason := <-e.cfg.Kill:
			e.log.Warn("kill switch tripped", "reason", reason)
			e.book.Kill(reason)
			if err := e.exits(ctx, e.clk.Now()); err != nil {
				return err
			}
		case <-advance:
			advance = e.clk.After(e.cfg.AdvanceEvery)
			if err := e.exits(ctx, e.clk.Now()); err != nil {
				return err
			}
			if err := e.advance(ctx); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) onEvent(ctx context.Context, ev marketdata.Event) error {
	var err error
	switch {
	case ev.Tick != nil:
		e.stats.Ticks++
		err = e.onTick(ctx, *ev.Tick)
	case ev.Bar != nil:
		e.stats.BarEvents++
		err = e.onBar(ctx, *ev.Bar)
	}
	if err != nil {
		return err
	}
	if e.cfg.WaitForDecisions {
		return e.await(ctx)
	}
	return nil
}

// eventTime is the time a tick happened.
func eventTime(t domain.Tick) time.Time {
	if t.ExchangeTime.IsZero() {
		return t.ReceivedAt
	}
	return t.ExchangeTime
}

func (e *Engine) onTick(ctx context.Context, t domain.Tick) error {
	ts := eventTime(t)
	if end := e.windowEnd(ts); !end.IsZero() && !ts.Before(end) {
		// Past the window only the bar builder sees the tick: it closes the
		// window's last bars, and the bar sink keeps the day's prices whole.
		for _, b := range e.builder.Add(t) {
			if err := e.onBarClosed(ctx, b); err != nil {
				return err
			}
		}
		return nil
	}
	e.moveClock(ts)
	if e.cfg.TickSink != nil {
		e.cfg.TickSink(t)
	}
	e.book.MarkAt(t.InstrumentToken, t.LastPrice, ts)
	// Orders placed on earlier events see this tick first, so a decision made
	// on it can never fill on it.
	if e.feed != nil {
		e.feed.OnTick(t)
		if err := e.drain(ctx); err != nil {
			return err
		}
	}
	// Exits first: they roll the book to a new day before any PnL is read.
	if err := e.exits(ctx, e.clk.Now()); err != nil {
		return err
	}
	for _, b := range e.builder.Add(t) {
		if err := e.onBarClosed(ctx, b); err != nil {
			return err
		}
	}
	return e.advance(ctx)
}

func (e *Engine) onBar(ctx context.Context, b domain.Bar) error {
	if end := e.windowEnd(b.Start); !end.IsZero() && b.End.After(end) {
		return e.onBarClosed(ctx, b) // the sink only; see Window
	}
	e.moveClock(b.End)
	e.book.MarkBar(b)
	if e.feed != nil {
		e.feed.OnBar(b)
		if err := e.drain(ctx); err != nil {
			return err
		}
	}
	if err := e.exits(ctx, e.clk.Now()); err != nil {
		return err
	}
	return e.onBarClosed(ctx, b)
}

// advance closes the bars that are due by the clock.
func (e *Engine) advance(ctx context.Context) error {
	for _, b := range e.builder.Advance(e.clk.Now()) {
		if err := e.onBarClosed(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// moveClock moves a simulated clock to t. A wall clock moves on its own.
func (e *Engine) moveClock(t time.Time) {
	if e.settable != nil && !t.IsZero() {
		e.settable.Set(t)
	}
}

func (e *Engine) result(rec orders.Reconciliation) Result {
	return Result{
		Stats:          e.stats,
		Positions:      e.book.Positions(),
		PnL:            e.carryPnL + e.book.PnL(),
		Realized:       e.carryRealized + e.book.RealizedPnL(),
		OpenOrders:     len(e.mgr.Open()),
		Reconciliation: rec,
	}
}
