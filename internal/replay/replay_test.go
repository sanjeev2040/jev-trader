package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/decide"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	mdreplay "github.com/sanjeev2040/jev-trader/internal/marketdata/replay"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

var (
	day   = time.Date(2026, 10, 5, 0, 0, 0, 0, risk.IST)
	infy  = domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "INFY"}
	nifty = domain.Instrument{Token: 99, Exchange: "NSE", Symbol: "NIFTY 50"}
	bg    = context.Background()
)

// minutePrices: one tick a minute from 09:15 at :30 past. The price rises,
// dips once so a limit entry can fill, then falls.
var minutePrices = []float64{
	1500, 1503, 1506, 1509, 1512, 1515, 1518, 1517, 1520, 1522, 1523,
	1520, 1515, 1510, 1505, 1500, 1498, 1497, 1496, 1495, 1494,
}

// writeFixture records the scenario's ticks the way `jevtrader record` does.
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	w := store.NewTickWriter(root, 0)
	for i, p := range minutePrices {
		ts := day.Add(9*time.Hour + time.Duration(15+i)*time.Minute + 30*time.Second).UTC()
		for _, in := range []struct {
			inst  domain.Instrument
			scale float64
		}{{nifty, 10}, {infy, 1}} {
			tk := domain.Tick{
				InstrumentToken: in.inst.Token, Symbol: in.inst.Exchange + ":" + in.inst.Symbol,
				ExchangeTime: ts, ReceivedAt: ts.Add(40 * time.Millisecond),
				LastPrice: p * in.scale, Volume: int64(1000 * (i + 1)),
			}
			if err := w.Write(tk); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func config(t *testing.T, root string, log *decisionlog.Log) Config {
	t.Helper()
	// These fixtures replay minutes of data, so they keep the pre-#53 churn rules.
	limits := risk.DefaultLimits()
	limits.MinHold, limits.ReentryCooldown, limits.RotationMinHold = 5*time.Minute, 10*time.Minute, 5*time.Minute
	limits.EntryConfirmations, limits.EntryMinProbability = 0, 0
	return Config{
		Log: log,
		NewSource: func() (marketdata.Source, error) {
			return mdreplay.New(mdreplay.Config{Root: root, Kind: mdreplay.Ticks})
		},
		SourceName: "ticks", Label: "fixture ticks",
		Limits: limits, Paper: paper.DefaultConfig(), Bars: bars.Config{},
		Snapshot:   snapshot.Config{SizeThresholds: [2]float64{20_000, 50_000}},
		IndexToken: nifty.Token, RunPrefix: "bt",
	}
}

func openLog(t *testing.T) *decisionlog.Log {
	t.Helper()
	l, err := decisionlog.Open(":memory:", decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func momentum(t *testing.T) decide.Decider {
	t.Helper()
	m, err := decide.NewMomentum(decide.MomentumConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func byDecider(rep *Report, name string) RunMetrics {
	for _, m := range rep.Runs {
		if m.Decider == name {
			return m
		}
	}
	return RunMetrics{}
}

// TestEndToEndReplayOverFixtureTicks replays recorded Parquet ticks through the
// engine with the baselines and the paper broker, then checks the decision log
// and the evaluation report. A third run answers from the momentum run's log.
func TestEndToEndReplayOverFixtureTicks(t *testing.T) {
	root := writeFixture(t)
	log := openLog(t)
	cfg := config(t, root, log)

	rep, err := Run(bg, cfg, []Spec{
		{Name: "hold", Decider: decide.AlwaysHold{}},
		{Name: "momentum", Decider: momentum(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Runs) != 2 || rep.Label != "fixture ticks" || rep.Horizon != DefaultHorizon {
		t.Fatalf("report = %+v", rep)
	}

	hold, mom := byDecider(rep, "hold"), byDecider(rep, "momentum")
	// The always-HOLD baseline decides on all 20 closed INFY bars and never trades.
	if hold.Cycles != 20 || hold.Holds != 20 || hold.Orders != 0 || hold.Trades != 0 || hold.NetPnL != 0 || hold.MaxDrawdown != 0 || hold.Turnover != 0 {
		t.Errorf("hold = %+v", hold)
	}
	// Momentum enters on the 09:21 bar and exits when the trend turns.
	if mom.Cycles != 20 || mom.Orders != 2 || mom.Fills != 2 || mom.Trades != 1 || mom.RiskExits != 0 {
		t.Fatalf("momentum = %+v", mom)
	}
	if mom.Buys == 0 || mom.Sells == 0 || mom.Buys+mom.Sells+mom.Holds != mom.Cycles {
		t.Errorf("momentum answers = %+v", mom)
	}
	if mom.Costs <= 0 || mom.Turnover <= 0 || math.Abs(mom.GrossPnL-(mom.NetPnL+mom.Costs)) > 1e-9 {
		t.Errorf("momentum costs/turnover = %+v", mom)
	}
	wantHit := 0.0
	if mom.Wins == 1 {
		wantHit = 1
	}
	if mom.HitRate != wantHit {
		t.Errorf("hit rate %v with %d wins of %d trades", mom.HitRate, mom.Wins, mom.Trades)
	}
	if mom.MaxDrawdown < 0 || (mom.NetPnL < 0 && mom.MaxDrawdown < -mom.NetPnL) {
		t.Errorf("drawdown %v inconsistent with PnL %v", mom.MaxDrawdown, mom.NetPnL)
	}
	cal := mom.Calibration
	n := 0
	for _, b := range cal.Bins {
		n += b.N
	}
	if cal.Samples == 0 || n != cal.Samples || cal.Brier < 0 || cal.Brier > 1 {
		t.Errorf("calibration = %+v", cal)
	}

	// The decision log holds every cycle and its order events, per run.
	for _, run := range []string{"bt-hold", "bt-momentum"} {
		cs, err := log.Cycles(bg, run)
		if err != nil || len(cs) != 20 {
			t.Fatalf("%s: %d cycles, %v", run, len(cs), err)
		}
	}
	events, _ := log.OrderEvents(bg, "bt-momentum")
	if len(events) != 4 {
		t.Errorf("momentum order events = %d, want OPEN and FILLED for two orders", len(events))
	}
	if ev, _ := log.OrderEvents(bg, "bt-hold"); len(ev) != 0 {
		t.Errorf("hold placed orders: %+v", ev)
	}

	// Regression replay: the momentum run's recorded answers reproduce it.
	recorded, err := log.RecordedDecider(bg, "bt-momentum")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunPrefix = "reg"
	reg, err := Run(bg, cfg, []Spec{{Name: "recorded", Decider: recorded, Notes: func() []string {
		if recorded.Misses() > 0 {
			return []string{"misses"}
		}
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got := reg.Runs[0]
	if recorded.Misses() != 0 || recorded.Remaining() != 0 || len(reg.Notes) != 0 {
		t.Errorf("recorded replay diverged: %d misses, %d unused, notes %v", recorded.Misses(), recorded.Remaining(), reg.Notes)
	}
	got.RunID, got.Decider = mom.RunID, mom.Decider
	if !reflect.DeepEqual(got, mom) {
		t.Errorf("recorded replay differs from the run it replays:\n got %+v\nwant %+v", got, mom)
	}

	// The report prints, with its caveat, and encodes.
	var text bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fixture ticks", "bt-hold", "bt-momentum", "net PnL", "Calibration", "optimistic upper bounds"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
	var js bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || !reflect.DeepEqual(&back, rep) {
		t.Errorf("JSON round trip: %v", err)
	}
}

func TestReplayIsDeterministic(t *testing.T) {
	root := writeFixture(t)
	var reports []*Report
	for i := 0; i < 2; i++ {
		rep, err := Run(bg, config(t, root, openLog(t)), []Spec{{Name: "momentum", Decider: momentum(t)}})
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, rep)
	}
	if !reflect.DeepEqual(reports[0], reports[1]) {
		t.Errorf("two replays differ:\n%+v\n%+v", reports[0], reports[1])
	}
}

func TestRunValidates(t *testing.T) {
	root := writeFixture(t)
	good := config(t, root, openLog(t))
	spec := []Spec{{Name: "hold", Decider: decide.AlwaysHold{}}}
	for name, edit := range map[string]func(*Config){
		"no log":    func(c *Config) { c.Log = nil },
		"no source": func(c *Config) { c.NewSource = nil },
		"no prefix": func(c *Config) { c.RunPrefix = "" },
		"bad risk":  func(c *Config) { c.Limits = risk.Limits{} },
	} {
		c := good
		edit(&c)
		if _, err := Run(bg, c, spec); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Run(bg, good, nil); err == nil {
		t.Error("no deciders: want an error")
	}
	// A run id used twice is refused rather than mixed.
	if _, err := Run(bg, good, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(bg, good, spec); err == nil {
		t.Error("a repeated run id was accepted")
	}
}

func fill(side domain.Action, qty int64, price, charges float64) domain.Fill {
	return domain.Fill{Instrument: infy, Side: side, Quantity: qty, Price: price, Charges: charges}
}

func TestRoundTrips(t *testing.T) {
	got := roundTrips([]domain.Fill{
		fill(domain.ActionBuy, 10, 100, 1),
		fill(domain.ActionBuy, 10, 110, 1), // average 105
		fill(domain.ActionSell, 20, 106, 2),
		fill(domain.ActionSell, 5, 50, 1), // opens a short that stays open
	})
	// 20 × (106 − 105) − 4 charges.
	if len(got) != 1 || math.Abs(got[0]-16) > 1e-9 {
		t.Errorf("round trips = %v, want [16]", got)
	}
	short := roundTrips([]domain.Fill{fill(domain.ActionSell, 10, 100, 0), fill(domain.ActionBuy, 10, 90, 0)})
	if len(short) != 1 || short[0] != 100 {
		t.Errorf("short round trip = %v, want [100]", short)
	}
	if got := roundTrips(nil); got != nil {
		t.Errorf("no fills: %v", got)
	}
}

func TestMaxDrawdown(t *testing.T) {
	for _, tc := range []struct {
		curve []float64
		want  float64
	}{
		{nil, 0},
		{[]float64{1, 2, 3}, 0},
		{[]float64{-5, -2}, 5}, // the curve starts at zero
		{[]float64{10, 4, 12, 3, 8}, 9},
	} {
		if got := maxDrawdown(tc.curve); got != tc.want {
			t.Errorf("maxDrawdown(%v) = %v, want %v", tc.curve, got, tc.want)
		}
	}
}

func TestCalibrate(t *testing.T) {
	cal := calibrate([]sample{{0.9, 1}, {0.95, 1}, {0.9, 0}, {0.65, 1}, {1.0, 1}})
	if cal.Samples != 5 {
		t.Fatalf("samples = %d", cal.Samples)
	}
	if want := (0.01 + 0.0025 + 0.81 + 0.1225 + 0) / 5; math.Abs(cal.Brier-want) > 1e-9 {
		t.Errorf("brier = %v, want %v", cal.Brier, want)
	}
	if len(cal.Bins) != 2 || cal.Bins[0].Lo != 0.6 || cal.Bins[0].N != 1 || cal.Bins[1].N != 4 || cal.Bins[1].Hi != 1 {
		t.Errorf("bins = %+v", cal.Bins)
	}
	if b := cal.Bins[1]; math.Abs(b.Realized-0.75) > 1e-9 || math.Abs(b.Predicted-0.9375) > 1e-9 {
		t.Errorf("top bin = %+v", b)
	}
	if got := calibrate(nil); got.Samples != 0 || got.Brier != 0 || got.Bins != nil {
		t.Errorf("empty = %+v", got)
	}
}

func TestScoreLooksAheadOnlyForEvaluation(t *testing.T) {
	mk := func(end time.Time, close float64) domain.Bar {
		return domain.Bar{Instrument: infy, Interval: time.Minute, Start: end.Add(-time.Minute), End: end, Close: close}
	}
	t0 := day.Add(9*time.Hour + 16*time.Minute).UTC()
	f := newForward([]domain.Bar{mk(t0.Add(2*time.Minute), 101), mk(t0, 100), mk(t0.Add(time.Minute), 99)})
	c := decisionlog.Cycle{Instrument: infy, BarClose: t0, Price: 100, Decision: &domain.Decision{Action: domain.ActionBuy, Probability: 0.7}}
	if s, ok := f.score(c, time.Minute, 2); !ok || s.outcome != 1 || s.p != 0.7 {
		t.Errorf("BUY up = %+v, %v", s, ok)
	}
	c.Decision = &domain.Decision{Action: domain.ActionSell, Probability: 0.7}
	if s, ok := f.score(c, time.Minute, 2); !ok || s.outcome != 0 {
		t.Errorf("SELL into a rise = %+v, %v", s, ok)
	}
	if _, ok := f.score(c, time.Minute, 3); ok {
		t.Error("a call with no bar that far ahead was scored")
	}
	c.Price = 0
	if _, ok := f.score(c, time.Minute, 2); ok {
		t.Error("a call with no price was scored")
	}
}

func windowConfig(t *testing.T, root string, log *decisionlog.Log) Config {
	t.Helper()
	cfg := config(t, root, log)
	cfg.Window = engine.Window{Start: 9*time.Hour + 20*time.Minute, End: 9*time.Hour + 30*time.Minute}
	cfg.Walkthrough = WalkAll
	cfg.Compare = true
	return cfg
}

func runWindow(t *testing.T) *Report {
	t.Helper()
	rep, err := Run(bg, windowConfig(t, writeFixture(t), openLog(t)), []Spec{
		{Name: "hold", Decider: decide.AlwaysHold{}},
		{Name: "momentum", Decider: momentum(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func walkOf(rep *Report, decider string) RunWalk {
	for _, w := range rep.Walkthrough {
		if w.Decider == decider {
			return w
		}
	}
	return RunWalk{}
}

// TestWindowedWalkthrough follows the momentum run through 09:20-09:30: every
// row sits in the window, the first decision already has warm-up features,
// the BUY is approved and filled, and the instrument's PnL ties to the run's.
func TestWindowedWalkthrough(t *testing.T) {
	rep := runWindow(t)
	if rep.Window != "09:20-09:30 IST" {
		t.Errorf("window = %q", rep.Window)
	}
	if hold := byDecider(rep, "hold"); hold.Cycles != 10 {
		t.Errorf("hold decided %d times in a 10-minute window, want 10", hold.Cycles)
	}

	w := walkOf(rep, "momentum")
	if len(w.Instruments) != 1 || w.Instruments[0].Instrument != "NSE:INFY" || w.Instruments[0].Token != infy.Token {
		t.Fatalf("instruments = %+v (the index must not be traded)", w.Instruments)
	}
	iw := w.Instruments[0]
	if iw.Decisions != 10 || iw.Buys+iw.Sells+iw.Holds != 10 || iw.Fills != 2 || iw.Position != 0 {
		t.Errorf("instrument summary = %+v", iw)
	}
	mom := byDecider(rep, "momentum")
	if math.Abs(iw.NetPnL-round2(mom.NetPnL)) > 0.011 {
		t.Errorf("instrument PnL %.2f != run PnL %.2f", iw.NetPnL, mom.NetPnL)
	}

	open, closeAt := day.Add(9*time.Hour+20*time.Minute), day.Add(9*time.Hour+31*time.Minute)
	var buy, fill *WalkRow
	for i, r := range iw.Rows {
		if r.Time.Before(open) || r.Time.After(closeAt) {
			t.Errorf("row at %s is outside the window", r.Time.In(risk.IST))
		}
		if i > 0 && r.Time.Before(iw.Rows[i-1].Time) {
			t.Errorf("rows out of order at %d", i)
		}
		switch {
		case r.Kind == RowDecision && r.Action == "BUY" && r.Order != nil && buy == nil:
			buy = &iw.Rows[i]
		case r.Kind == RowFill && fill == nil:
			fill = &iw.Rows[i]
		}
	}
	if buy == nil || fill == nil {
		t.Fatalf("no approved BUY (%v) or fill (%v) in the walk", buy, fill)
	}
	if buy.Verdict != "approved" || buy.Order.Side != "BUY" || buy.Order.Quantity == 0 || buy.Probability <= 0 {
		t.Errorf("BUY row = %+v", buy)
	}
	f := buy.Features
	if f == nil || f.MinutesSinceOpen != 6 || f.Return5Pct == nil || f.IndexSessionPct == nil || f.VWAPDistPct == nil {
		t.Errorf("BUY features = %+v; want warm-up bars to count", f)
	}
	if fill.Side != "BUY" || fill.Quantity != buy.Order.Quantity || fill.Price <= 0 || fill.Position != fill.Quantity || fill.Charges <= 0 || fill.PnL >= 0 {
		t.Errorf("fill row = %+v", fill)
	}
	// A refused call carries the gate's reason.
	var refused bool
	for _, r := range iw.Rows {
		if r.Kind == RowDecision && r.Verdict == "order_in_flight" && r.VerdictDetail != "" {
			refused = true
		}
	}
	if !refused {
		t.Error("no refused decision with its reason in the walk")
	}

	// "actions" leaves out the HOLD rows of the always-HOLD run only.
	cfg := windowConfig(t, writeFixture(t), openLog(t))
	cfg.Walkthrough = WalkActions
	rep2, err := Run(bg, cfg, []Spec{{Name: "hold", Decider: decide.AlwaysHold{}}, {Name: "momentum", Decider: momentum(t)}})
	if err != nil {
		t.Fatal(err)
	}
	hw := walkOf(rep2, "hold").Instruments[0]
	if len(hw.Rows) != 0 || hw.Holds != 10 {
		t.Errorf("hold walk under actions = %d rows, %d holds", len(hw.Rows), hw.Holds)
	}
	if mw := walkOf(rep2, "momentum").Instruments[0]; len(mw.Rows) != len(iw.Rows) {
		t.Errorf("momentum has no HOLDs, so %d rows should stay %d", len(mw.Rows), len(iw.Rows))
	}
}

// TestComparisonWithTheMarketThatFollowed checks the real prices after each
// call (the data continues past the window), buy-and-hold, and the baselines.
func TestComparisonWithTheMarketThatFollowed(t *testing.T) {
	rep := runWindow(t)
	cmp := rep.Comparison
	if cmp == nil || fmt.Sprint(cmp.LookaheadMinutes) != "[5 15 30]" {
		t.Fatalf("comparison = %+v", cmp)
	}
	var first *CallOutcome
	for i, c := range cmp.Calls {
		if c.Decider == "hold" {
			t.Fatalf("a HOLD is not a call: %+v", c)
		}
		if i == 0 {
			first = &cmp.Calls[0]
		}
	}
	if first == nil {
		t.Fatal("no calls")
	}
	// The 09:21 BUY: the 09:20 bar closed at 1515. Five minutes on, the 09:25
	// bar closed at 1523; the window ended at 1505 and the data's last bar
	// closed at 1495. The data stops before 15 and 30 minutes pass.
	labels := []string{"+5m", "+15m", "+30m", "window end", "day close"}
	if first.Action != "BUY" || first.Price != 1515 || len(first.Outcomes) != len(labels) || !first.Executed || first.Verdict != "approved" {
		t.Fatalf("first call = %+v", first)
	}
	for i, o := range first.Outcomes {
		if o.Label != labels[i] {
			t.Errorf("outcome %d = %q, want %q", i, o.Label, labels[i])
		}
	}
	want := map[string]float64{"+5m": 1523, "window end": 1505, "day close": 1495}
	for _, o := range first.Outcomes {
		w, scored := want[o.Label]
		if scored != (o.Price != nil) || (scored && (*o.Price != w || math.Abs(*o.MovePct-(w-1515)/1515*100) > 1e-9)) {
			t.Errorf("outcome %+v, want price %v (scored: %v)", o, w, scored)
		}
	}
	// A SELL is right when the price falls.
	for _, c := range cmp.Calls {
		if c.Action == "SELL" && c.Time.Equal(day.Add(9*time.Hour+28*time.Minute)) {
			if m := c.Outcomes[len(c.Outcomes)-1].MovePct; m == nil || *m <= 0 {
				t.Errorf("SELL at 09:28 (price %v) closed at %v: move %v, want positive", c.Price, 1495, m)
			}
		}
	}
	var fiveMinute *CallSummary
	for i, s := range cmp.Summary {
		if s.Label == "+5m" {
			fiveMinute = &cmp.Summary[i]
		}
	}
	if fiveMinute == nil || fiveMinute.Calls != 10 || fiveMinute.Scored != 10 || fiveMinute.Right != 5 {
		t.Errorf("+5m summary = %+v", fiveMinute)
	}

	// Buy-and-hold buys 13 INFY at the 09:20 bar's open and holds to 09:30.
	bh := cmp.BuyHold
	if len(bh.Rows) != 1 || bh.Notional != 20_000 {
		t.Fatalf("buy-and-hold = %+v", bh)
	}
	row := bh.Rows[0]
	if row.Quantity != 13 || row.StartPrice != 1515 || row.EndPrice != 1505 || row.Gross != -130 || row.DayClose == nil || *row.DayClose != 1495 {
		t.Errorf("buy-and-hold row = %+v", row)
	}
	if row.Costs <= 0 || math.Abs(row.Net-(row.Gross-row.Costs)) > 0.011 || bh.Net != row.Net {
		t.Errorf("buy-and-hold costs/net = %+v", bh)
	}

	// Strategy against the benchmarks.
	var holdRun, momRun RunPnL
	for _, p := range cmp.PnL {
		switch p.Decider {
		case "hold":
			holdRun = p
		case "momentum":
			momRun = p
		}
	}
	if !holdRun.Baseline || !momRun.Baseline || holdRun.NetPnL != 0 {
		t.Errorf("pnl rows = %+v / %+v", holdRun, momRun)
	}
	if momRun.VsHold == nil || *momRun.VsHold != round2(momRun.NetPnL) || momRun.VsMomentum == nil || *momRun.VsMomentum != 0 ||
		math.Abs(momRun.VsBuyHold-round2(momRun.NetPnL-bh.Net)) > 0.011 {
		t.Errorf("momentum vs benchmarks = %+v", momRun)
	}
}

func TestReportTextAndJSONCarryWalkthroughAndComparison(t *testing.T) {
	rep := runWindow(t)
	var text bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Trading window: 09:20-09:30 IST", "Walkthrough: momentum (bt-momentum)", "NSE:INFY: 10 decisions",
		"DECIDE BUY  p=0.75", "FILL   BUY", "gate order_in_flight", "Comparison with the real market",
		"BUY/SELL calls, bt-momentum", "window end", "day close", "Buy-and-hold benchmark", "vs buy-and-hold",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q", want)
		}
	}

	var js bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Walkthrough) != 2 || back.Comparison == nil || len(back.Comparison.Calls) == 0 || len(back.Comparison.PnL) != 2 || back.Window == "" {
		t.Errorf("JSON report lost the walkthrough or comparison: %s", js.String())
	}
}

// TestReportWithoutTheNewOptionsIsUnchanged keeps the existing output exactly:
// no window line, no walkthrough, no comparison.
func TestReportWithoutTheNewOptionsIsUnchanged(t *testing.T) {
	rep, err := Run(bg, config(t, writeFixture(t), openLog(t)), []Spec{{Name: "hold", Decider: decide.AlwaysHold{}}})
	if err != nil {
		t.Fatal(err)
	}
	var text, js bytes.Buffer
	_ = rep.WriteText(&text)
	_ = rep.WriteJSON(&js)
	for _, banned := range []string{"Trading window", "Walkthrough", "Comparison with"} {
		if strings.Contains(text.String(), banned) {
			t.Errorf("text report has %q", banned)
		}
	}
	for _, banned := range []string{`"window"`, `"walkthrough"`, `"comparison"`} {
		if strings.Contains(js.String(), banned) {
			t.Errorf("JSON report has %s", banned)
		}
	}
}

func TestPriceLookups(t *testing.T) {
	at := func(m int) time.Time { return day.Add(9*time.Hour + time.Duration(m)*time.Minute) }
	bar := func(endMin int, close float64) domain.Bar {
		return domain.Bar{Instrument: infy, Interval: time.Minute, Start: at(endMin - 1), End: at(endMin), Close: close}
	}
	// 09:20 has no bar: a quiet minute.
	tape := newForward([]domain.Bar{bar(16, 10), bar(17, 11), bar(19, 13), bar(21, 15)})
	for _, tc := range []struct {
		target int
		want   float64
		ok     bool
	}{
		{17, 11, true}, // an exact bar
		{20, 13, true}, // a quiet minute carries the last close forward
		{21, 15, true}, // the last bar
		{22, 0, false}, // beyond the data
		{16, 10, true}, // the call's own bar
	} {
		got, ok := tape.priceAt(infy.Token, at(16), at(tc.target))
		if ok != tc.ok || got != tc.want {
			t.Errorf("priceAt(%d) = %v, %v; want %v, %v", tc.target, got, ok, tc.want, tc.ok)
		}
	}
	if p, ok := tape.closeAt(infy.Token, at(18)); !ok || p != 11 {
		t.Errorf("closeAt(18) = %v, %v", p, ok)
	}
	if _, ok := tape.closeAt(infy.Token, at(10)); ok {
		t.Error("closeAt before the first bar should be unknown")
	}
	if got := dayClose(tape, infy.Token, at(16)); !got.Equal(at(21)) {
		t.Errorf("dayClose = %v", got)
	}
}
