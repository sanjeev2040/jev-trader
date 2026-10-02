package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/clock"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
)

var (
	// day is a Monday, in IST.
	day   = time.Date(2026, 10, 5, 0, 0, 0, 0, risk.IST)
	infy  = domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}
	nifty = domain.Instrument{Token: 99, Exchange: "NSE", Symbol: "NIFTY 50"}
	bg    = context.Background()
)

func at(h, m, s int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second).UTC()
}

// tickAt is a tick of in at h:m:s; volume grows with the clock so bars have it.
func tickAt(in domain.Instrument, h, m, s int, price float64) domain.Tick {
	ts := at(h, m, s)
	return domain.Tick{
		InstrumentToken: in.Token, Symbol: in.Exchange + ":" + in.Symbol,
		ExchangeTime: ts, ReceivedAt: ts, LastPrice: price,
		Volume: int64(h*3600+m*60+s) * 10,
	}
}

// minutePrices has one tick per minute from 09:15 at :30 past the minute. The
// price rises, dips once so a limit entry can fill, then falls.
var minutePrices = []float64{
	1500, 1503, 1506, 1509, 1512, 1515, 1518, 1517, 1520, 1522, 1523,
	1520, 1515, 1510, 1505, 1500, 1498, 1497, 1496, 1495, 1494,
}

func scenarioTicks(in domain.Instrument, scale float64) []marketdata.Event {
	var evs []marketdata.Event
	for i, p := range minutePrices {
		t := tickAt(in, 9, 15+i, 30, p*scale)
		evs = append(evs, marketdata.Event{Tick: &t})
	}
	return evs
}

type sliceSource struct{ events []marketdata.Event }

func (s sliceSource) Run(ctx context.Context, out chan<- marketdata.Event) error {
	for _, ev := range s.events {
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type rig struct {
	log    *decisionlog.Log
	sim    *clock.Sim
	broker *paper.Broker
	cfg    Config
}

func newRig(t *testing.T, events []marketdata.Event, d decide.Decider) *rig {
	t.Helper()
	log, err := decisionlog.Open(":memory:", decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	sim := clock.NewSim(time.Time{})
	broker, err := paper.New(sim, paper.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	gate, err := risk.NewGate(risk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return &rig{log: log, sim: sim, broker: broker, cfg: Config{
		RunID: "run1", Mode: "replay", DeciderName: "test", SourceName: "fixture",
		Source: sliceSource{events}, Clock: sim, Decider: d, Broker: broker, Gate: gate, Log: log,
		Snapshot:         snapshot.Config{SizeThresholds: [2]float64{20_000, 50_000}},
		WaitForDecisions: true,
	}}
}

func (r *rig) run(t *testing.T) Result {
	t.Helper()
	e, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(bg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func momentum(t *testing.T) decide.Decider {
	t.Helper()
	m, err := decide.NewMomentum(decide.MomentumConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func cyclesOf(t *testing.T, l *decisionlog.Log, run string) []decisionlog.Cycle {
	t.Helper()
	cs, err := l.Cycles(bg, run)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func eventsOf(t *testing.T, l *decisionlog.Log, run string) []decisionlog.OrderEvent {
	t.Helper()
	es, err := l.OrderEvents(bg, run)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func countVerdict(cs []decisionlog.Cycle, verdict string) int {
	n := 0
	for _, c := range cs {
		if c.Verdict == verdict {
			n++
		}
	}
	return n
}

func TestMomentumRunLogsEveryCycleAndTradesOnce(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	res := r.run(t)

	cs := cyclesOf(t, r.log, "run1")
	// 21 ticks close 20 one-minute bars; each is one decision cycle.
	if len(cs) != 20 || res.Cycles != 20 || res.Bars != 20 {
		t.Fatalf("cycles %d (stats %d), bars %d; want 20", len(cs), res.Cycles, res.Bars)
	}
	for i, c := range cs {
		if want := fmt.Sprintf("run1-c%06d", i+1); c.CycleID != want {
			t.Errorf("cycle %d id = %q, want %q", i, c.CycleID, want)
		}
		if c.Source != decisionlog.SourceDecision || c.Decision == nil || c.Snapshot == nil || c.Alias != "I01" {
			t.Errorf("cycle %d = %+v", i, c)
		}
		if c.SchemaVersion != snapshot.SchemaVersion || strings.Contains(string(c.Snapshot), "INFY") {
			t.Errorf("cycle %d snapshot leaks or is unversioned: %s", i, c.Snapshot)
		}
		if !c.AppliedAt.Equal(c.AppliedAt.UTC()) || c.AppliedAt.Before(c.BarClose) {
			t.Errorf("cycle %d applied %v before bar close %v", i, c.AppliedAt, c.BarClose)
		}
	}
	if got := cs[0].BarClose; !got.Equal(at(9, 16, 0)) {
		t.Errorf("first bar close = %v, want 09:16 IST", got)
	}

	// One entry and one exit; the short signal after the exit is refused.
	if n := countVerdict(cs, string(risk.ReasonApproved)); n != 2 {
		t.Fatalf("approved cycles = %d, want 2 (entry and exit)", n)
	}
	if countVerdict(cs, string(risk.ReasonShortNotAllowed)) == 0 {
		t.Error("expected the gate to refuse a short entry")
	}
	var entry, exit decisionlog.Cycle
	for _, c := range cs {
		if c.Intent != nil {
			if entry.CycleID == "" {
				entry = c
			} else {
				exit = c
			}
		}
	}
	if entry.Intent.Side != domain.ActionBuy || entry.Intent.ReduceOnly || entry.Intent.Quantity != 6 {
		t.Errorf("entry intent = %+v, want BUY 6", entry.Intent)
	}
	if !entry.BarClose.Equal(at(9, 21, 0)) {
		t.Errorf("entry bar close = %v, want 09:21 IST", entry.BarClose)
	}
	if exit.Intent.Side != domain.ActionSell || !exit.Intent.ReduceOnly || exit.Intent.Quantity != 6 {
		t.Errorf("exit intent = %+v, want reduce-only SELL 6", exit.Intent)
	}

	// Order events: acknowledged, then filled after the decision, never on the
	// tick that closed the bar.
	evs := eventsOf(t, r.log, "run1")
	var statuses []string
	var fills []decisionlog.OrderEvent
	for _, e := range evs {
		statuses = append(statuses, e.CycleID[len(e.CycleID)-4:]+":"+e.Status)
		if e.Fill != nil {
			fills = append(fills, e)
		}
	}
	if len(evs) != 4 || len(fills) != 2 {
		t.Fatalf("order events %v, fills %d; want OPEN+FILLED for two orders", statuses, len(fills))
	}
	if !fills[0].Time.After(entry.BarClose) || !fills[0].Time.Equal(at(9, 22, 30)) {
		t.Errorf("entry filled at %v, want the first tick after the decision (09:22:30 IST)", fills[0].Time)
	}
	if fills[0].Fill.Charges <= 0 || fills[0].Costs != fills[0].Fill.Charges {
		t.Errorf("entry fill carries no costs: %+v", fills[0])
	}
	if res.Fills != 2 || res.OrdersPlaced != 2 || res.Exits != 0 || len(res.Positions) != 0 || res.OpenOrders != 0 {
		t.Errorf("result = %+v", res)
	}

	// The run lost the round trip's costs and the spread, and the equity series
	// ends at the result's PnL.
	eq, err := r.log.Equity(bg, "run1")
	if err != nil || len(eq) != 20 {
		t.Fatalf("equity = %d points, %v; want one per bar-close time", len(eq), err)
	}
	if last := eq[len(eq)-1]; last.PnL != res.PnL || last.Realized != res.Realized {
		t.Errorf("last equity %+v, result PnL %v realized %v", last, res.PnL, res.Realized)
	}
	if res.Realized >= fills[1].Fill.Price*6 {
		t.Errorf("implausible realized PnL %v", res.Realized)
	}
}

func TestAlwaysHoldNeverTrades(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), decide.AlwaysHold{})
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if len(cs) != 20 || countVerdict(cs, string(risk.ReasonHold)) != 20 {
		t.Errorf("cycles %d, hold verdicts %d", len(cs), countVerdict(cs, string(risk.ReasonHold)))
	}
	if len(eventsOf(t, r.log, "run1")) != 0 || res.OrdersPlaced != 0 || res.PnL != 0 || res.Holds != 20 || res.HoldSubstitutions != 0 {
		t.Errorf("result = %+v", res)
	}
}

type failingDecider struct{ err error }

func (f failingDecider) Decide(context.Context, json.RawMessage) (domain.Decision, error) {
	return domain.Decision{}, f.err
}

func TestDeciderFailureIsALoggedHold(t *testing.T) {
	err := &domain.DecideError{Reason: domain.ReasonHTTP5xx, Err: errors.New("boom")}
	r := newRig(t, scenarioTicks(infy, 1), failingDecider{err})
	r.cfg.Decider = failingDecider{err}
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if len(cs) != 20 || res.HoldSubstitutions != 20 {
		t.Fatalf("cycles %d, substitutions %d", len(cs), res.HoldSubstitutions)
	}
	for i, c := range cs {
		if c.Decision.Action != domain.ActionHold || c.Decision.HoldReason != domain.ReasonHTTP5xx || c.Verdict != string(risk.ReasonHold) {
			t.Fatalf("cycle %d = %+v, want a HOLD with the failure's reason", i, c.Decision)
		}
	}
	if res.OrdersPlaced != 0 {
		t.Error("a failed decider placed an order")
	}
}

func TestOrderNeverPrecedesItsLogRow(t *testing.T) {
	// The decider closes the log, so the cycle row cannot be written.
	r := newRig(t, scenarioTicks(infy, 1), nil)
	r.cfg.Decider = deciderFunc(func(context.Context, json.RawMessage) (domain.Decision, error) {
		r.log.Close()
		return domain.Decision{Action: domain.ActionBuy, Size: domain.SizeS, Probability: 0.9}, nil
	})
	e, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(bg)
	if err == nil {
		t.Fatal("Run succeeded although the decision log was unwritable")
	}
	orders, _ := r.broker.Orders(bg)
	if len(orders) != 0 {
		t.Errorf("%d orders reached the broker without a log row", len(orders))
	}
}

type deciderFunc func(context.Context, json.RawMessage) (domain.Decision, error)

func (f deciderFunc) Decide(ctx context.Context, s json.RawMessage) (domain.Decision, error) {
	return f(ctx, s)
}

func TestKillSwitchBlocksEntriesAndFlattens(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	book := risk.NewBook()
	book.Kill(risk.ReasonKillFile)
	r.cfg.Book = book
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if countVerdict(cs, string(risk.ReasonKillSwitch)) == 0 || res.OrdersPlaced != 0 {
		t.Errorf("kill switch: %d refusals, %d orders", countVerdict(cs, string(risk.ReasonKillSwitch)), res.OrdersPlaced)
	}
}

func TestSquareOffFlattensWithoutTheDecider(t *testing.T) {
	// Long from 09:21; HOLD forever after; the 15:10 square-off must close it.
	var evs []marketdata.Event
	for _, ev := range scenarioTicks(infy, 1)[:9] { // rising: the entry fills
		evs = append(evs, ev)
	}
	for _, tk := range []domain.Tick{
		tickAt(infy, 15, 9, 30, 1522), tickAt(infy, 15, 10, 30, 1521), tickAt(infy, 15, 11, 30, 1520), tickAt(infy, 15, 12, 30, 1519),
	} {
		tk := tk
		evs = append(evs, marketdata.Event{Tick: &tk})
	}
	calls := 0
	d := deciderFunc(func(ctx context.Context, s json.RawMessage) (domain.Decision, error) {
		if calls++; calls == 6 { // the bar that closes at 09:21, inside the entry window
			return domain.Decision{Action: domain.ActionBuy, Size: domain.SizeS, Probability: 0.9,
				Probabilities: map[domain.Action]float64{domain.ActionBuy: 0.9, domain.ActionHold: 0.1}}, nil
		}
		return decide.AlwaysHold{}.Decide(ctx, s)
	})
	r := newRig(t, evs, d)
	res := r.run(t)

	cs := cyclesOf(t, r.log, "run1")
	var exits []decisionlog.Cycle
	for _, c := range cs {
		if c.Source == decisionlog.SourceRisk {
			exits = append(exits, c)
		}
	}
	if len(exits) != 1 || res.Exits != 1 {
		t.Fatalf("risk exits = %d (stats %d), want 1", len(exits), res.Exits)
	}
	x := exits[0]
	if x.ExitReason != string(risk.ReasonSquareOff) || x.Decision != nil || x.Snapshot != nil || !x.Intent.ReduceOnly || x.Intent.Side != domain.ActionSell {
		t.Errorf("exit cycle = %+v", x)
	}
	if !x.BarClose.Equal(at(15, 10, 30)) {
		t.Errorf("exit at %v, want the first tick at or after 15:10 IST", x.BarClose)
	}
	if len(res.Positions) != 0 || res.OpenOrders != 0 {
		t.Errorf("not flat at the end: %+v", res)
	}
	// The exit's order event follows its cycle row, and fills at the next tick.
	var exitFill *decisionlog.OrderEvent
	for _, e := range eventsOf(t, r.log, "run1") {
		if e.CycleID == x.CycleID && e.Fill != nil {
			e := e
			exitFill = &e
		}
	}
	if exitFill == nil || !exitFill.Time.Equal(at(15, 11, 30)) {
		t.Errorf("exit fill = %+v, want at 15:11:30", exitFill)
	}
}

func TestIndexContextReachesTheSnapshotAndIsNotTraded(t *testing.T) {
	var evs []marketdata.Event
	infyEvs, idxEvs := scenarioTicks(infy, 1), scenarioTicks(nifty, 10)
	for i := range infyEvs {
		evs = append(evs, idxEvs[i], infyEvs[i])
	}
	r := newRig(t, evs, momentum(t))
	r.cfg.IndexToken = nifty.Token
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if len(cs) != 20 {
		t.Fatalf("cycles = %d, want INFY's 20 only", len(cs))
	}
	for _, c := range cs {
		if c.Instrument.Token != infy.Token {
			t.Fatalf("the index was traded: %+v", c.Instrument)
		}
	}
	var s snapshot.Snapshot
	if err := json.Unmarshal(cs[10].Snapshot, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Index.Available {
		t.Errorf("snapshot has no index context: %s", cs[10].Snapshot)
	}
	// The index bar of the same minute may still be open: never a bar from
	// after the decision, which Build rejects as look-ahead.
	if res.SnapshotErrors != 0 {
		t.Errorf("snapshot errors = %d", res.SnapshotErrors)
	}
}

func TestPreOpenTicksAreNotDecisionBars(t *testing.T) {
	var evs []marketdata.Event
	for _, tk := range []domain.Tick{tickAt(infy, 9, 5, 30, 1490), tickAt(infy, 9, 6, 30, 1491), tickAt(infy, 9, 15, 30, 1500), tickAt(infy, 9, 16, 30, 1501)} {
		tk := tk
		evs = append(evs, marketdata.Event{Tick: &tk})
	}
	r := newRig(t, evs, decide.AlwaysHold{})
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if res.SnapshotErrors != 0 {
		t.Errorf("snapshot errors = %d", res.SnapshotErrors)
	}
	for _, c := range cs {
		if c.BarClose.Before(at(9, 16, 0)) {
			t.Errorf("decision on a pre-open bar closing %v", c.BarClose)
		}
	}
	if len(cs) != 1 {
		t.Errorf("cycles = %d, want 1 (the 09:15 bar)", len(cs))
	}
}

func TestBarEventsDriveTheSamePath(t *testing.T) {
	var evs []marketdata.Event
	for i, p := range minutePrices {
		start := at(9, 15+i, 0)
		b := domain.Bar{Instrument: infy, Interval: time.Minute, Start: start, End: start.Add(time.Minute),
			Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 1000}
		evs = append(evs, marketdata.Event{Bar: &b})
	}
	r := newRig(t, evs, momentum(t))
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if len(cs) != len(minutePrices) || res.BarEvents != len(minutePrices) || res.Ticks != 0 {
		t.Fatalf("cycles %d, stats %+v", len(cs), res.Stats)
	}
	fills := 0
	for _, e := range eventsOf(t, r.log, "run1") {
		if e.Fill != nil {
			fills++
			// A bar fills an order only if it starts at or after the order time.
			var cycle decisionlog.Cycle
			for _, c := range cs {
				if c.CycleID == e.CycleID {
					cycle = c
				}
			}
			if e.Time.Before(cycle.AppliedAt) {
				t.Errorf("fill at %v before the decision applied at %v", e.Time, cycle.AppliedAt)
			}
		}
	}
	if fills == 0 {
		t.Error("no order filled on bar data")
	}
}

func TestLatencyOverrideDelaysTheOrder(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	r.cfg.Latency = 90 * time.Second
	r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	applied := map[string]time.Time{}
	for _, c := range cs {
		if c.Decision.Latency != 90*time.Second || !c.AppliedAt.Equal(c.BarClose.Add(90*time.Second)) && c.AppliedAt.Before(c.BarClose.Add(90*time.Second)) {
			t.Fatalf("cycle %s latency %v applied %v bar close %v", c.CycleID, c.Decision.Latency, c.AppliedAt, c.BarClose)
		}
		applied[c.CycleID] = c.AppliedAt
	}
	fills := 0
	for _, e := range eventsOf(t, r.log, "run1") {
		if e.Fill != nil {
			fills++
			// The entry is decided on the 09:21 bar, applied at 09:22:30: the
			// 09:22:30 tick that would have filled it with no latency is too early.
			if !e.Time.After(applied[e.CycleID]) {
				t.Errorf("fill at %v is not after the decision applied at %v", e.Time, applied[e.CycleID])
			}
			if e.Fill.Side == domain.ActionBuy && e.Time.Equal(at(9, 22, 30)) {
				t.Error("latency did not delay the entry")
			}
		}
	}
	if fills == 0 {
		t.Error("nothing filled")
	}
}

func TestAsynchronousDecisionsComplete(t *testing.T) {
	// Without WaitForDecisions the loop keeps reading while deciders answer.
	slow := deciderFunc(func(ctx context.Context, s json.RawMessage) (domain.Decision, error) {
		time.Sleep(2 * time.Millisecond)
		return decide.AlwaysHold{}.Decide(ctx, s)
	})
	r := newRig(t, scenarioTicks(infy, 1), slow)
	r.cfg.WaitForDecisions = false
	res := r.run(t)
	cs := cyclesOf(t, r.log, "run1")
	if len(cs)+res.BusySkips != 20 || len(cs) == 0 {
		t.Errorf("cycles %d + busy skips %d, want 20 bars", len(cs), res.BusySkips)
	}
}

func TestRunsAreIsolatedInTheSharedLog(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	r.run(t)
	// A second run through the same log, with its own broker, reuses nothing.
	sim := clock.NewSim(time.Time{})
	broker, _ := paper.New(sim, paper.DefaultConfig())
	cfg := r.cfg
	cfg.RunID, cfg.Clock, cfg.Broker = "run2", sim, broker
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(bg); err != nil {
		t.Fatal(err)
	}
	a, b := cyclesOf(t, r.log, "run1"), cyclesOf(t, r.log, "run2")
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("runs have %d and %d cycles", len(a), len(b))
	}
	runs, _ := r.log.Runs(bg)
	if len(runs) != 2 {
		t.Errorf("runs = %+v", runs)
	}
}

func TestMultiDayEquityCarriesOver(t *testing.T) {
	// Day one trades and ends flat with a loss; day two does nothing. The
	// equity series must not reset to zero when the risk book rolls its day.
	evs := scenarioTicks(infy, 1)
	nextDay := day.AddDate(0, 0, 1)
	for i, p := range minutePrices[:3] {
		ts := nextDay.Add(9*time.Hour + time.Duration(15+i)*time.Minute + 30*time.Second).UTC()
		tk := domain.Tick{InstrumentToken: infy.Token, Symbol: "NSE:INFY", ExchangeTime: ts, ReceivedAt: ts, LastPrice: p, Volume: int64(100 * (i + 1))}
		evs = append(evs, marketdata.Event{Tick: &tk})
	}
	r := newRig(t, evs, momentum(t))
	res := r.run(t)
	eq, err := r.log.Equity(bg, "run1")
	if err != nil {
		t.Fatal(err)
	}
	var dayOneEnd float64
	for _, p := range eq {
		if p.Time.Before(nextDay.UTC()) {
			dayOneEnd = p.PnL
		}
	}
	if dayOneEnd == 0 || eq[len(eq)-1].PnL != dayOneEnd || res.PnL != dayOneEnd {
		t.Errorf("day one ended at %v, last point %v, result %v", dayOneEnd, eq[len(eq)-1].PnL, res.PnL)
	}
}

func TestNewValidates(t *testing.T) {
	r := newRig(t, nil, decide.AlwaysHold{})
	good := r.cfg
	for name, edit := range map[string]func(*Config){
		"no run id":    func(c *Config) { c.RunID = "" },
		"no source":    func(c *Config) { c.Source = nil },
		"no decider":   func(c *Config) { c.Decider = nil },
		"no gate":      func(c *Config) { c.Gate = nil },
		"bad snapshot": func(c *Config) { c.Snapshot = snapshot.Config{} },
		"negative":     func(c *Config) { c.Latency = -1 },
	} {
		c := good
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
