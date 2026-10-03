package decisionlog

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

var (
	ist  = time.FixedZone("IST", 5*3600+1800)
	t0   = time.Date(2026, 10, 5, 9, 16, 0, 0, ist).UTC()
	infy = domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}
	bg   = context.Background()
)

func openMem(t *testing.T) *Log {
	t.Helper()
	l, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	if err := l.StartRun(bg, Run{ID: "r1", Mode: "replay", Decider: "momentum", Source: "ticks", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	return l
}

func decision() *domain.Decision {
	return &domain.Decision{
		Action: domain.ActionBuy, Size: domain.SizeM, Probability: 0.7,
		Probabilities:     map[domain.Action]float64{domain.ActionBuy: 0.7, domain.ActionHold: 0.3},
		SizeProbabilities: map[string]float64{domain.SizeM: 1},
		Confidence:        0.8, Model: "jev-test-1", InputTokens: 120, OutputTokens: 9,
		Latency: 230 * time.Millisecond, Raw: json.RawMessage(`{"answers":[{"x": 1}]}`),
	}
}

func intent(cycleID string) *domain.OrderIntent {
	return &domain.OrderIntent{CycleID: cycleID, Instrument: infy, Side: domain.ActionBuy, Quantity: 10, RefPrice: 1500.5}
}

func cycle(id string, snapshot string, d *domain.Decision, in *domain.OrderIntent) Cycle {
	c := Cycle{
		CycleID: id, RunID: "r1", Source: SourceDecision, Mode: "replay", Instrument: infy, Alias: "I01",
		BarClose: t0, Price: 1500, AppliedAt: t0.Add(230 * time.Millisecond),
		SchemaVersion: 1, Snapshot: json.RawMessage(snapshot), Decision: d,
		Verdict: "approved", Intent: in,
	}
	if in != nil {
		c.ClientID = "jt" + id
	}
	return c
}

func TestCycleRoundTrip(t *testing.T) {
	l := openMem(t)
	want := cycle("r1-c1", `{"schema_version":1,"instrument":"I01"}`, decision(), intent("r1-c1"))
	if err := l.AppendCycle(bg, want); err != nil {
		t.Fatal(err)
	}
	exit := Cycle{
		CycleID: "risk-square_off-1", RunID: "r1", Source: SourceRisk, Mode: "replay", Instrument: infy, Alias: "I01",
		BarClose: t0.Add(time.Hour), Price: 1510, AppliedAt: t0.Add(time.Hour), ExitReason: "square_off", Verdict: "square_off",
		Intent:   &domain.OrderIntent{CycleID: "risk-square_off-1", Instrument: infy, Side: domain.ActionSell, Quantity: 10, RefPrice: 1510, ReduceOnly: true},
		ClientID: "jtrisk",
	}
	if err := l.AppendCycle(bg, exit); err != nil {
		t.Fatal(err)
	}

	got, err := l.Cycles(bg, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cycles", len(got))
	}
	want.Seq, exit.Seq = got[0].Seq, got[1].Seq
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("decision cycle round trip:\n got %+v\nwant %+v", got[0], want)
	}
	if !reflect.DeepEqual(got[1], exit) {
		t.Errorf("exit cycle round trip:\n got %+v\nwant %+v", got[1], exit)
	}
	if got[0].Seq >= got[1].Seq {
		t.Errorf("cycles out of write order: %d, %d", got[0].Seq, got[1].Seq)
	}
	if other, _ := l.Cycles(bg, "nope"); len(other) != 0 {
		t.Errorf("other run has %d cycles", len(other))
	}
}

func TestCycleIDsAreUnique(t *testing.T) {
	l := openMem(t)
	c := cycle("r1-c1", `{}`, decision(), nil)
	if err := l.AppendCycle(bg, c); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendCycle(bg, c); err == nil {
		t.Error("a repeated cycle id was accepted")
	}
	// The same id in another run is fine: a risk exit's id repeats across replays.
	if err := l.StartRun(bg, Run{ID: "r2", Mode: "replay", Decider: "hold", Source: "ticks", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	c.RunID = "r2"
	if err := l.AppendCycle(bg, c); err != nil {
		t.Errorf("same cycle id in another run: %v", err)
	}
	c.CycleID, c.RunID = "r2-c1", "unknown-run"
	if err := l.AppendCycle(bg, c); err == nil {
		t.Error("a cycle for an unknown run was accepted")
	}
}

func TestLogIsAppendOnly(t *testing.T) {
	l := openMem(t)
	if err := l.AppendCycle(bg, cycle("r1-c1", `{}`, decision(), intent("r1-c1"))); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendOrderEvent(bg, OrderEvent{RunID: "r1", CycleID: "r1-c1", ClientID: "jt1", Instrument: infy, Side: domain.ActionBuy, Status: "OPEN", Time: t0}); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendEquity(bg, "r1", EquityPoint{Time: t0}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE runs SET note = 'x'`, `DELETE FROM runs`,
		`UPDATE cycles SET verdict = 'x'`, `DELETE FROM cycles`,
		`UPDATE order_events SET status = 'x'`, `DELETE FROM order_events`,
		`UPDATE equity SET pnl = 1`, `DELETE FROM equity`,
	} {
		if _, err := l.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want an append-only refusal", stmt, err)
		}
	}
}

func TestRecordIntentRequiresTheCycleRow(t *testing.T) {
	l := openMem(t)
	in := intent("r1-c1")
	if err := l.Journal("r1").RecordIntent(bg, *in); !errors.Is(err, ErrIntentNotLogged) {
		t.Fatalf("before the cycle is logged: err = %v", err)
	}
	// A cycle without an order does not authorise one.
	if err := l.AppendCycle(bg, cycle("r1-c1", `{}`, decision(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := l.Journal("r1").RecordIntent(bg, *in); !errors.Is(err, ErrIntentNotLogged) {
		t.Fatalf("cycle logged without an intent: err = %v", err)
	}
	if err := l.AppendCycle(bg, cycle("r1-c2", `{}`, decision(), intent("r1-c2"))); err != nil {
		t.Fatal(err)
	}
	if err := l.Journal("r1").RecordIntent(bg, *intent("r1-c2")); err != nil {
		t.Errorf("logged intent: %v", err)
	}
	changed := *intent("r1-c2")
	changed.Quantity++
	if err := l.Journal("r1").RecordIntent(bg, changed); !errors.Is(err, ErrIntentNotLogged) {
		t.Errorf("an intent that differs from the logged one: err = %v", err)
	}
}

func TestIntentsSince(t *testing.T) {
	l := openMem(t)
	if err := l.StartRun(bg, Run{ID: "r2", Mode: "paper", Decider: "jev", Source: "kite", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	old := cycle("r1-c1", `{}`, decision(), intent("r1-c1"))
	old.AppliedAt = t0.Add(-48 * time.Hour)
	fresh := cycle("r1-c2", `{}`, decision(), intent("r1-c2"))
	noOrder := cycle("r1-c3", `{}`, decision(), nil)
	otherMode := cycle("r2-c1", `{}`, decision(), intent("r2-c1"))
	otherMode.RunID, otherMode.Mode = "r2", "paper"
	for _, c := range []Cycle{old, fresh, noOrder, otherMode} {
		if err := l.AppendCycle(bg, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.IntentsSince(bg, "replay", t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CycleID != "r1-c2" {
		t.Errorf("got %+v, want only r1-c2", got)
	}
}

func TestOrderEventsAndEquityRoundTrip(t *testing.T) {
	l := openMem(t)
	if err := l.AppendCycle(bg, cycle("r1-c1", `{}`, decision(), intent("r1-c1"))); err != nil {
		t.Fatal(err)
	}
	events := []OrderEvent{
		{RunID: "r1", CycleID: "r1-c1", ClientID: "jt1", BrokerID: "paper-1", Instrument: infy, Side: domain.ActionBuy, Status: "OPEN", Time: t0},
		{RunID: "r1", CycleID: "r1-c1", ClientID: "jt1", BrokerID: "paper-1", Instrument: infy, Side: domain.ActionBuy, Status: "FILLED",
			FilledQuantity: 10, AveragePrice: 1500.1, Costs: 7.5, Time: t0.Add(time.Second),
			Fill: &domain.Fill{ClientID: "jt1", Instrument: infy, Side: domain.ActionBuy, Quantity: 10, Price: 1500.1, Charges: 7.5, Time: t0.Add(time.Second)}},
		{RunID: "r1", CycleID: "r1-c1", ClientID: "jt1", Instrument: infy, Side: domain.ActionBuy, Status: StatusPlaceFailed, Reason: "boom", Time: t0.Add(2 * time.Second)},
	}
	for _, e := range events {
		if err := l.AppendOrderEvent(bg, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.OrderEvents(bg, "r1")
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		events[i].Seq = got[i].Seq
	}
	if !reflect.DeepEqual(got, events) {
		t.Errorf("order events:\n got %+v\nwant %+v", got, events)
	}

	pts := []EquityPoint{{Time: t0, PnL: 0}, {Time: t0.Add(time.Minute), PnL: -12.5, Realized: -7.5}}
	for _, p := range pts {
		if err := l.AppendEquity(bg, "r1", p); err != nil {
			t.Fatal(err)
		}
	}
	gotEq, err := l.Equity(bg, "r1")
	if err != nil || !reflect.DeepEqual(gotEq, pts) {
		t.Errorf("equity = %+v, %v", gotEq, err)
	}
}

func TestRunsAndSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "decisions.db")
	// Open does not create directories; the caller does.
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("opening in a missing directory should fail")
	}
	path = filepath.Join(t.TempDir(), "decisions.db")
	l, err := Open(path, Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.StartRun(bg, Run{ID: "a", Mode: "replay", Decider: "hold", Source: "ticks", Note: "n", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := l.StartRun(bg, Run{ID: "a"}); err == nil {
		t.Error("a repeated run id was accepted")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening keeps the data and the schema.
	l, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := l.Runs(bg)
	if err != nil || len(runs) != 1 || runs[0].ID != "a" || runs[0].Note != "n" || !runs[0].StartedAt.Equal(t0) {
		t.Errorf("runs = %+v, %v", runs, err)
	}
	if _, err := l.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := Open(path, Options{}); !errors.Is(err, ErrSchemaVersion) {
		t.Errorf("future schema: err = %v", err)
	}
	if _, err := Open(path, Options{Synchronous: "OFF"}); err == nil {
		t.Error("synchronous OFF was accepted")
	}
}

func TestRecordedDecider(t *testing.T) {
	l := openMem(t)
	first, second := decision(), decision()
	second.Action, second.Probability = domain.ActionSell, 0.65
	hold := &domain.Decision{Action: domain.ActionHold, HoldReason: domain.ReasonTimeout, Probabilities: map[domain.Action]float64{}}
	for _, c := range []Cycle{
		cycle("r1-c1", `{"s":1}`, first, nil),
		cycle("r1-c2", `{"s":1}`, second, nil), // an identical snapshot, answered second
		cycle("r1-c3", `{"s":2}`, hold, nil),
	} {
		if err := l.AppendCycle(bg, c); err != nil {
			t.Fatal(err)
		}
	}
	// A risk exit has no snapshot and is not an answer.
	if err := l.AppendCycle(bg, Cycle{CycleID: "risk-1", RunID: "r1", Source: SourceRisk, Mode: "replay", Instrument: infy, Verdict: "square_off", BarClose: t0}); err != nil {
		t.Fatal(err)
	}

	r, err := l.RecordedDecider(bg, "r1")
	if err != nil {
		t.Fatal(err)
	}
	ask := func(state string) (domain.Decision, error) { return r.Decide(bg, json.RawMessage(state)) }

	d, err := ask(`{"s":1}`)
	if err != nil || !reflect.DeepEqual(d, *first) {
		t.Errorf("first answer = %+v, %v", d, err)
	}
	if d, err = ask(`{"s":1}`); err != nil || d.Action != domain.ActionSell {
		t.Errorf("second answer = %+v, %v", d, err)
	}
	if d, err = ask(`{"s":2}`); err != nil || d.HoldReason != domain.ReasonTimeout {
		t.Errorf("recorded HOLD = %+v, %v", d, err)
	}
	if r.Remaining() != 0 || r.Misses() != 0 {
		t.Errorf("remaining %d, misses %d", r.Remaining(), r.Misses())
	}
	_, err = ask(`{"s":1}`)
	var de *domain.DecideError
	if !errors.Is(err, ErrNoRecordedAnswer) || !errors.As(err, &de) || de.Reason != domain.ReasonError {
		t.Errorf("exhausted: err = %v", err)
	}
	if _, err = ask(`{"s":3}`); err == nil || r.Misses() != 2 {
		t.Errorf("unknown snapshot: err = %v, misses %d", err, r.Misses())
	}
	if _, err := l.RecordedDecider(bg, "nope"); err == nil {
		t.Error("a run with no decisions should be refused")
	}
}
