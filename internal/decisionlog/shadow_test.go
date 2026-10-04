package decisionlog

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func shadowSignal(signal string, at time.Time) ShadowSignal {
	return ShadowSignal{
		RunID: "r1", Day: at.In(ist).Format(time.DateOnly), Kind: ShadowDecision, Signal: signal, Instrument: infy,
		RefTime: at, RefPrice: 1500.5, WindowEnd: at.Add(5 * time.Hour), IndexToken: 256265, Direction: 1,
		CycleID: "r1-c000001", Action: "BUY", Probability: 0.8, Verdict: "approved", VerdictDetail: "size S", Traded: true,
	}
}

func TestShadowSignalRoundTrip(t *testing.T) {
	l := openMem(t)
	buy := shadowSignal("decision:BUY", t0)
	pick := ShadowSignal{RunID: "r1", Day: "2026-10-05", Kind: ShadowPick, Signal: "pick", Instrument: infy,
		RefTime: t0.Add(-time.Minute), Direction: 1, Detail: json.RawMessage(`{"rank":1,"score":0.5}`)}
	for _, s := range []ShadowSignal{buy, pick} {
		if ok, err := l.AppendShadowSignal(bg, s); err != nil || !ok {
			t.Fatalf("append %s = %v, %v", s.Signal, ok, err)
		}
	}
	got, err := l.ShadowSignals(bg, "2026-10-05", "2026-10-05")
	if err != nil || len(got) != 2 {
		t.Fatalf("signals = %+v, %v", got, err)
	}
	buy.ID, pick.ID = 1, 2
	if !reflect.DeepEqual(got[0], buy) {
		t.Errorf("decision signal = %+v, want %+v", got[0], buy)
	}
	if got[1].ID != 2 || got[1].Kind != ShadowPick || string(got[1].Detail) != `{"rank":1,"score":0.5}` || !got[1].WindowEnd.IsZero() || got[1].Traded {
		t.Errorf("pick signal = %+v", got[1])
	}
	// The day range is inclusive on both ends.
	for _, c := range []struct {
		from, to string
		n        int
	}{{"2026-10-04", "2026-10-04", 0}, {"2026-10-04", "2026-10-05", 2}, {"2026-10-05", "2026-10-06", 2}, {"2026-10-06", "2026-10-07", 0}} {
		if got, _ := l.ShadowSignals(bg, c.from, c.to); len(got) != c.n {
			t.Errorf("%s..%s: %d signals, want %d", c.from, c.to, len(got), c.n)
		}
	}
}

func TestShadowSignalIsLoggedOncePerDayKindInstrumentAndTime(t *testing.T) {
	l := openMem(t)
	if err := l.StartRun(bg, Run{ID: "r2", Mode: "paper", Decider: "jev", Source: "kite", StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	s := shadowSignal("pick", t0)
	s.Kind = ShadowPick
	if ok, err := l.AppendShadowSignal(bg, s); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	// A restart the same day logs the same pick again, under another run.
	s.RunID = "r2"
	if ok, err := l.AppendShadowSignal(bg, s); err != nil || ok {
		t.Errorf("repeat = %v, %v; want it ignored", ok, err)
	}
	if got, _ := l.ShadowSignals(bg, "2026-10-05", "2026-10-05"); len(got) != 1 || got[0].RunID != "r1" {
		t.Errorf("signals = %+v", got)
	}
	// The same instrument a minute later is another signal.
	s.RefTime = t0.Add(time.Minute)
	if ok, err := l.AppendShadowSignal(bg, s); err != nil || !ok {
		t.Errorf("later = %v, %v", ok, err)
	}
}

func TestShadowSignalNeedsItsKeyFieldsAndARun(t *testing.T) {
	l := openMem(t)
	for name, mutate := range map[string]func(*ShadowSignal){
		"no run":    func(s *ShadowSignal) { s.RunID = "" },
		"no day":    func(s *ShadowSignal) { s.Day = "" },
		"no kind":   func(s *ShadowSignal) { s.Kind = "" },
		"no label":  func(s *ShadowSignal) { s.Signal = "" },
		"no time":   func(s *ShadowSignal) { s.RefTime = time.Time{} },
		"ghost run": func(s *ShadowSignal) { s.RunID = "nope" },
	} {
		s := shadowSignal("decision:BUY", t0)
		mutate(&s)
		if _, err := l.AppendShadowSignal(bg, s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestShadowOutcomesRoundTripAndAreKeptOnRefill(t *testing.T) {
	l := openMem(t)
	if _, err := l.AppendShadowSignal(bg, shadowSignal("decision:BUY", t0)); err != nil {
		t.Fatal(err)
	}
	idx, uni := 0.3, 0.1
	o := ShadowOutcome{SignalID: 1, Horizon: "+15m", Target: t0.Add(15 * time.Minute), RefPrice: 100, Price: 101, ReturnPct: 1,
		IndexPct: &idx, UniversePct: &uni, UniverseNames: 40, FilledAt: t0.Add(time.Hour)}
	if err := l.AppendShadowOutcome(bg, o); err != nil {
		t.Fatal(err)
	}
	bare := ShadowOutcome{SignalID: 1, Horizon: "day_close", Target: t0.Add(5 * time.Hour), RefPrice: 100, Price: 99, ReturnPct: -1, FilledAt: t0.Add(time.Hour)}
	if err := l.AppendShadowOutcome(bg, bare); err != nil {
		t.Fatal(err)
	}
	// A second outcome for the same signal and horizon is ignored.
	again := o
	again.Price, again.ReturnPct = 200, 100
	if err := l.AppendShadowOutcome(bg, again); err != nil {
		t.Fatal(err)
	}
	got, err := l.ShadowOutcomes(bg, "2026-10-05", "2026-10-05")
	if err != nil || len(got) != 2 {
		t.Fatalf("outcomes = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0], o) {
		t.Errorf("outcome = %+v, want %+v", got[0], o)
	}
	if got[1].IndexPct != nil || got[1].UniversePct != nil {
		t.Errorf("a missing benchmark was read as %v, %v", got[1].IndexPct, got[1].UniversePct)
	}
	if other, _ := l.ShadowOutcomes(bg, "2026-10-06", "2026-10-06"); len(other) != 0 {
		t.Errorf("outcomes of another day = %+v", other)
	}
	if err := l.AppendShadowOutcome(bg, ShadowOutcome{SignalID: 77, Horizon: "+15m", FilledAt: t0}); err == nil {
		t.Error("an outcome for no signal was accepted")
	}
}

// A log written before the shadow tables existed opens, keeps what it holds
// and gains the new tables.
func TestVersionOneLogMigratesToTheShadowTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{schema, appendOnly, `PRAGMA user_version = 1`,
		`INSERT INTO runs (run_id, mode, decider, source, started_at) VALUES ('old', 'paper', 'jev', 'kite', 1)`,
		`INSERT INTO cycles (cycle_id, run_id, source, mode, token, exchange, symbol, alias, bar_close, price, applied_at, verdict)
			VALUES ('old-c1', 'old', 'jev', 'paper', 1, 'NSE', 'INFY', 'I01', 2, 1500, 3, 'hold')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%.40s: %v", stmt, err)
		}
	}
	db.Close()

	l, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("a version 1 log did not open: %v", err)
	}
	var version int
	if err := l.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Errorf("user_version = %d, %v; want %d", version, err, SchemaVersion)
	}
	if cs, err := l.Cycles(bg, "old"); err != nil || len(cs) != 1 || cs[0].CycleID != "old-c1" {
		t.Errorf("old cycles = %+v, %v", cs, err)
	}
	s := shadowSignal("decision:BUY", t0)
	s.RunID = "old"
	if ok, err := l.AppendShadowSignal(bg, s); err != nil || !ok {
		t.Fatalf("shadow signal on a migrated log = %v, %v", ok, err)
	}
	if _, err := l.db.Exec(`DELETE FROM shadow_signals`); err == nil {
		t.Error("the migrated shadow table is not append-only")
	}
	l.Close()

	// Reopening a migrated log is a no-op.
	l, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got, _ := l.ShadowSignals(bg, "2026-10-05", "2026-10-05"); len(got) != 1 {
		t.Errorf("signals after reopen = %d", len(got))
	}
}
