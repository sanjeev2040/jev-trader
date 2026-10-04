package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
)

func shadowOf(t *testing.T, l *decisionlog.Log) []decisionlog.ShadowSignal {
	t.Helper()
	s, err := l.ShadowSignals(bg, day.Format(time.DateOnly), day.Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func shadowConfig() *Shadow {
	return &Shadow{
		Picks:    []ShadowPick{{Instrument: infy, Detail: json.RawMessage(`{"rank":1,"score":0.6}`)}},
		PickTime: 9*time.Hour + 20*time.Minute, WindowEnd: 15 * time.Hour,
	}
}

// The shadow log only adds rows: the cycles, orders, equity and counters of a
// run are the same with it on and off.
func TestShadowLogDoesNotChangeTheRun(t *testing.T) {
	off := newRig(t, scenarioTicks(infy, 1), momentum(t))
	resOff := off.run(t)

	on := newRig(t, scenarioTicks(infy, 1), momentum(t))
	on.cfg.Shadow = shadowConfig()
	resOn := on.run(t)

	if !reflect.DeepEqual(resOn, resOff) {
		t.Errorf("result with the shadow log = %+v, without = %+v", resOn, resOff)
	}
	csOn, csOff := cyclesOf(t, on.log, "run1"), cyclesOf(t, off.log, "run1")
	if !reflect.DeepEqual(csOn, csOff) {
		t.Errorf("cycles differ with the shadow log on")
	}
	if !reflect.DeepEqual(eventsOf(t, on.log, "run1"), eventsOf(t, off.log, "run1")) {
		t.Errorf("order events differ with the shadow log on")
	}
	eqOn, _ := on.log.Equity(bg, "run1")
	eqOff, _ := off.log.Equity(bg, "run1")
	if !reflect.DeepEqual(eqOn, eqOff) {
		t.Errorf("equity differs with the shadow log on")
	}
	if got := shadowOf(t, off.log); len(got) != 0 {
		t.Errorf("a run without Shadow logged %d signals", len(got))
	}
}

func TestShadowLogsEveryDecisionWithItsVerdictAndThePicks(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	r.cfg.Shadow = shadowConfig()
	r.cfg.IndexToken = nifty.Token
	res := r.run(t)
	if res.ShadowErrors != 0 {
		t.Fatalf("shadow errors = %d", res.ShadowErrors)
	}
	cs := cyclesOf(t, r.log, "run1")
	sigs := shadowOf(t, r.log)

	var picks, decisions []decisionlog.ShadowSignal
	for _, s := range sigs {
		switch s.Kind {
		case decisionlog.ShadowPick:
			picks = append(picks, s)
		case decisionlog.ShadowDecision:
			decisions = append(decisions, s)
		default:
			t.Errorf("signal kind %q", s.Kind)
		}
	}

	if len(picks) != 1 {
		t.Fatalf("picks = %+v", picks)
	}
	p := picks[0]
	if p.Signal != "pick" || p.Instrument != infy || p.Direction != 1 || string(p.Detail) != `{"rank":1,"score":0.6}` ||
		!p.RefTime.Equal(at(9, 20, 0)) || !p.WindowEnd.Equal(at(15, 0, 0)) || p.RunID != "run1" || p.Day != "2026-10-05" {
		t.Errorf("pick = %+v", p)
	}

	// One signal per cycle, in the same order, saying what the cycle says.
	if len(decisions) != len(cs) {
		t.Fatalf("decision signals = %d, cycles = %d", len(decisions), len(cs))
	}
	traded, refused := 0, 0
	for i, c := range cs {
		s := decisions[i]
		if s.CycleID != c.CycleID || s.Action != string(c.Decision.Action) || s.Verdict != c.Verdict ||
			s.VerdictDetail != c.VerdictDetail || s.Probability != c.Decision.Probability ||
			s.RefPrice != c.Price || !s.RefTime.Equal(c.BarClose) || s.Instrument != infy ||
			s.IndexToken != nifty.Token || !s.WindowEnd.Equal(at(15, 0, 0)) {
			t.Errorf("signal %d = %+v does not match cycle %+v", i, s, c)
		}
		if s.Traded != (c.Intent != nil) {
			t.Errorf("signal %d traded = %v, cycle intent = %v", i, s.Traded, c.Intent)
		}
		if want := "decision:" + string(c.Decision.Action); s.Signal != want {
			t.Errorf("signal %d label = %q, want %q", i, s.Signal, want)
		}
		wantDir := 1
		if c.Decision.Action == domain.ActionSell {
			wantDir = -1
		}
		if s.Direction != wantDir {
			t.Errorf("signal %d direction = %d for %s", i, s.Direction, c.Decision.Action)
		}
		if s.Traded {
			traded++
		} else if c.Decision.Action != domain.ActionHold {
			refused++
		}
	}
	// The momentum run enters once and exits once, and the gate refuses a short:
	// the shadow log holds the refused signals as well.
	if traded != 2 || refused == 0 {
		t.Errorf("traded = %d, refused BUY/SELL = %d", traded, refused)
	}
}

func TestShadowLabelsASubstitutedHoldAsFailed(t *testing.T) {
	err := &domain.DecideError{Reason: domain.ReasonHTTP5xx, Err: errors.New("boom")}
	r := newRig(t, scenarioTicks(infy, 1), failingDecider{err})
	r.cfg.Shadow = &Shadow{}
	r.run(t)
	sigs := shadowOf(t, r.log)
	if len(sigs) != 20 {
		t.Fatalf("signals = %d", len(sigs))
	}
	for _, s := range sigs {
		if s.Signal != "decision:FAILED" || s.Action != "HOLD" || s.Traded {
			t.Fatalf("signal = %+v", s)
		}
	}
}

func TestShadowWithoutAWindowEndOrPickTime(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	r.cfg.Shadow = &Shadow{Picks: []ShadowPick{{Instrument: infy}}}
	r.run(t)
	for _, s := range shadowOf(t, r.log) {
		if !s.WindowEnd.IsZero() {
			t.Fatalf("window end = %v, want none", s.WindowEnd)
		}
		if s.Kind == decisionlog.ShadowPick && s.RefTime.IsZero() {
			t.Fatal("a pick has no reference time")
		}
	}
}

// A failing shadow write is counted and logged and goes no further: the
// shadow log is evidence, not part of the trading path.
func TestShadowWriteFailureIsCountedNotReturned(t *testing.T) {
	r := newRig(t, scenarioTicks(infy, 1), momentum(t))
	r.cfg.Shadow = &Shadow{}
	e, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.shadowAppend(bg, decisionlog.ShadowSignal{}) // no run, day or kind: the log refuses it
	if e.stats.ShadowErrors != 1 {
		t.Errorf("shadow errors = %d, want 1", e.stats.ShadowErrors)
	}
}

// A day's picks are logged once, on its first decision bar, and a second day
// in the same run logs its own.
func TestShadowPicksAreLoggedOncePerDay(t *testing.T) {
	r := newRig(t, append(scenarioTicks(infy, 1), nextDayTicks(infy)...), momentum(t))
	r.cfg.Shadow = shadowConfig()
	r.run(t)
	var days []string
	for _, d := range []time.Time{day, day.AddDate(0, 0, 1)} {
		s, err := r.log.ShadowSignals(bg, d.Format(time.DateOnly), d.Format(time.DateOnly))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, sig := range s {
			if sig.Kind == decisionlog.ShadowPick {
				n++
				days = append(days, sig.Day)
			}
		}
		if n != 1 {
			t.Errorf("%s: %d picks, want 1", d.Format(time.DateOnly), n)
		}
	}
	if len(days) != 2 {
		t.Errorf("pick days = %v", days)
	}
}

// nextDayTicks is the scenario again the next day.
func nextDayTicks(in domain.Instrument) []marketdata.Event {
	var evs []marketdata.Event
	for i, p := range minutePrices {
		ts := day.AddDate(0, 0, 1).Add(9*time.Hour + time.Duration(15+i)*time.Minute + 30*time.Second).UTC()
		t := domain.Tick{InstrumentToken: in.Token, Symbol: in.Exchange + ":" + in.Symbol, ExchangeTime: ts, ReceivedAt: ts, LastPrice: p, Volume: int64(i+1) * 1000}
		evs = append(evs, marketdata.Event{Tick: &t})
	}
	return evs
}
