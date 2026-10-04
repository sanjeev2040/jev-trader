package shadow

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/store"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

var bg = context.Background()

var (
	stockA = domain.Instrument{Token: 1, Exchange: "NSE", Symbol: "AAA"}
	stockB = domain.Instrument{Token: 2, Exchange: "NSE", Symbol: "BBB"}
	stockC = domain.Instrument{Token: 3, Exchange: "NSE", Symbol: "CCC"}
	nifty  = domain.Instrument{Token: 99, Exchange: "NSE", Symbol: "NIFTY 50"}
)

// at is h:m IST on the given day (YYYY-MM-DD).
func at(day string, h, m int) time.Time {
	d, err := time.ParseInLocation(time.DateOnly, day, ist)
	if err != nil {
		panic(err)
	}
	return d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).UTC()
}

// minuteBars are one-minute bars of in from 09:16 to 15:30, the bar ending
// 09:15+k minutes closing at price(k); k = 0 is the 09:15 bar (ending 09:15).
func minuteBars(day string, in domain.Instrument, price func(k int) float64) []domain.Bar {
	var out []domain.Bar
	for k := 0; k <= 375; k++ {
		end := at(day, 9, 15).Add(time.Duration(k) * time.Minute)
		p := price(k)
		out = append(out, domain.Bar{Instrument: in, Interval: time.Minute, Start: end.Add(-time.Minute), End: end,
			Open: p, High: p, Low: p, Close: p, Volume: 100})
	}
	return out
}

func writeBars(t *testing.T, dir, kind string, bars ...[]domain.Bar) {
	t.Helper()
	w, err := store.NewBarWriter(dir, kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, bs := range bars {
		if err := w.Write(bs...); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func openLog(t *testing.T) *decisionlog.Log {
	t.Helper()
	l, err := decisionlog.Open(":memory:", decisionlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	if err := l.StartRun(bg, decisionlog.Run{ID: "r1", Mode: "paper", Decider: "jev", Source: "kite", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return l
}

func addSignal(t *testing.T, l *decisionlog.Log, s decisionlog.ShadowSignal) {
	t.Helper()
	s.RunID = "r1"
	if s.Day == "" {
		s.Day = s.RefTime.In(ist).Format(time.DateOnly)
	}
	if ok, err := l.AppendShadowSignal(bg, s); err != nil || !ok {
		t.Fatalf("AppendShadowSignal = %v, %v", ok, err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFillReadsHorizonsFromTheRecordedBars(t *testing.T) {
	const day = "2026-10-05"
	dir := t.TempDir()
	// A rises 0.1 a minute from 100, B is flat, C falls; the index rises 0.2 a minute from 1000.
	writeBars(t, dir, store.KindBars,
		minuteBars(day, stockA, func(k int) float64 { return 100 + 0.1*float64(k) }),
		minuteBars(day, stockB, func(int) float64 { return 200 }),
		minuteBars(day, stockC, func(k int) float64 { return 50 - 0.05*float64(k) }),
		minuteBars(day, nifty, func(k int) float64 { return 1000 + 0.2*float64(k) }),
	)
	l := openLog(t)
	ref := at(day, 9, 20)
	end := at(day, 15, 0)
	for _, in := range []domain.Instrument{stockA, stockB, stockC} {
		addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowPick, Signal: "pick", Instrument: in,
			RefTime: ref, WindowEnd: end, IndexToken: nifty.Token, Direction: 1})
	}
	now := at(day, 16, 0)
	res, err := Fill(bg, l, dir, day, day, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Signals != 3 || res.Filled != 12 || res.Pending != 0 {
		t.Fatalf("Fill = %+v, want 3 signals, 12 outcomes, none pending", res)
	}
	sigs, _ := l.ShadowSignals(bg, day, day)
	outs, err := l.ShadowOutcomes(bg, day, day)
	if err != nil || len(outs) != 12 {
		t.Fatalf("outcomes = %d, %v", len(outs), err)
	}
	get := func(id int64, h string) decisionlog.ShadowOutcome {
		for _, o := range outs {
			if o.SignalID == id && o.Horizon == h {
				return o
			}
		}
		t.Fatalf("no outcome %d %s", id, h)
		return decisionlog.ShadowOutcome{}
	}

	// A at +15m: bar 5 (09:20) closes 100.5, bar 20 (09:35) 102.
	o := get(sigs[0].ID, Horizon15m)
	wantA := (102.0 - 100.5) / 100.5 * 100
	wantIdx := (1004.0 - 1001.0) / 1001.0 * 100
	if !o.Target.Equal(at(day, 9, 35)) || !near(o.RefPrice, 100.5) || !near(o.Price, 102) || !near(o.ReturnPct, wantA) {
		t.Errorf("A +15m = %+v, want return %.6f", o, wantA)
	}
	if o.IndexPct == nil || !near(*o.IndexPct, wantIdx) {
		t.Errorf("A +15m index = %v, want %.6f", o.IndexPct, wantIdx)
	}
	// The universe is the three stocks, not the index: mean of A, B (0) and C.
	wantC := (50 - 0.05*20 - (50 - 0.05*5)) / (50 - 0.05*5) * 100
	if o.UniverseNames != 3 || o.UniversePct == nil || !near(*o.UniversePct, (wantA+0+wantC)/3) {
		t.Errorf("A +15m universe = %v over %d names, want %.6f", o.UniversePct, o.UniverseNames, (wantA+wantC)/3)
	}
	if o := get(sigs[1].ID, Horizon60m); !near(o.ReturnPct, 0) || !o.FilledAt.Equal(now) {
		t.Errorf("flat B +60m = %+v", o)
	}
	// Window end 15:00 is bar 345, the close 15:30 bar 375.
	if o := get(sigs[0].ID, HorizonWindow); !near(o.Price, 100+0.1*345) || !o.Target.Equal(end) {
		t.Errorf("A window end = %+v", o)
	}
	if o := get(sigs[2].ID, HorizonDayEnd); !near(o.Price, 50-0.05*375) || !o.Target.Equal(at(day, 15, 30)) {
		t.Errorf("C day close = %+v", o)
	}

	// Filling again changes nothing.
	again, err := Fill(bg, l, dir, day, day, now.Add(time.Hour))
	if err != nil || again.Filled != 0 || again.Pending != 0 {
		t.Errorf("second Fill = %+v, %v", again, err)
	}
	if outs2, _ := l.ShadowOutcomes(bg, day, day); len(outs2) != 12 {
		t.Errorf("outcomes after a refill = %d", len(outs2))
	}
}

func TestFillLeavesWhatTheBarsDoNotReachYet(t *testing.T) {
	const day = "2026-10-05"
	dir := t.TempDir()
	// The recording stops at 10:00.
	cut := func(bs []domain.Bar) []domain.Bar { return bs[:46] }
	writeBars(t, dir, store.KindBars,
		cut(minuteBars(day, stockA, func(k int) float64 { return 100 + float64(k) })),
		cut(minuteBars(day, nifty, func(k int) float64 { return 1000 })),
	)
	l := openLog(t)
	addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowDecision, Signal: "decision:BUY", Instrument: stockA,
		RefTime: at(day, 9, 30), RefPrice: 115, WindowEnd: at(day, 15, 0), IndexToken: nifty.Token, Direction: 1, Action: "BUY"})
	res, err := Fill(bg, l, dir, day, day, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// +15m (09:45) is covered; +60m, the window end and the close are not.
	if res.Filled != 1 || res.Pending != 3 {
		t.Fatalf("Fill = %+v, want 1 written, 3 pending", res)
	}
	// The stored reference price is used as written (115 is bar 15's close).
	outs, _ := l.ShadowOutcomes(bg, day, day)
	if len(outs) != 1 || !near(outs[0].RefPrice, 115) || !near(outs[0].ReturnPct, (130.0-115)/115*100) {
		t.Errorf("outcomes = %+v", outs)
	}

	// The rest of the day arrives (a backfill); the next Fill completes it, and
	// keeps the outcome already written.
	writeBars(t, dir, store.KindBackfill,
		minuteBars(day, stockA, func(k int) float64 { return 100 + float64(k) }),
		minuteBars(day, nifty, func(k int) float64 { return 1000 }),
	)
	res, err = Fill(bg, l, dir, day, day, time.Now())
	if err != nil || res.Filled != 3 || res.Pending != 0 {
		t.Fatalf("Fill after the backfill = %+v, %v", res, err)
	}
}

func TestFillSkipsHorizonsThatCannotApply(t *testing.T) {
	const day = "2026-10-05"
	dir := t.TempDir()
	writeBars(t, dir, store.KindBars, minuteBars(day, stockA, func(k int) float64 { return 100 + float64(k) }))
	l := openLog(t)
	// A 14:50 signal: +15m is 15:05, +60m is past the close, and there is no window end.
	addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowDecision, Signal: "decision:SELL", Instrument: stockA,
		RefTime: at(day, 14, 50), Direction: -1, Action: "SELL"})
	res, err := Fill(bg, l, dir, day, day, time.Now())
	if err != nil || res.Filled != 2 || res.Pending != 0 {
		t.Fatalf("Fill = %+v, %v; want +15m and the close only", res, err)
	}
	outs, _ := l.ShadowOutcomes(bg, day, day)
	if len(outs) != 2 || outs[0].Horizon != Horizon15m || outs[1].Horizon != HorizonDayEnd {
		t.Errorf("outcomes = %+v", outs)
	}
	// No index token: no index return, and the universe is the stock itself.
	if outs[0].IndexPct != nil || outs[0].UniverseNames != 1 {
		t.Errorf("benchmarks = %+v", outs[0])
	}
}

func TestFillKeepsAnIlliquidStocksLastPrice(t *testing.T) {
	const day = "2026-10-05"
	dir := t.TempDir()
	// B trades only until 09:40; A's bars carry the day to the close.
	writeBars(t, dir, store.KindBars,
		minuteBars(day, stockA, func(k int) float64 { return 100 }),
		minuteBars(day, stockB, func(k int) float64 { return 200 + float64(k) })[:26],
	)
	l := openLog(t)
	addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowPick, Signal: "pick", Instrument: stockB,
		RefTime: at(day, 9, 20), Direction: 1})
	if _, err := Fill(bg, l, dir, day, day, time.Now()); err != nil {
		t.Fatal(err)
	}
	outs, _ := l.ShadowOutcomes(bg, day, day)
	var closeOut *decisionlog.ShadowOutcome
	for i := range outs {
		if outs[i].Horizon == HorizonDayEnd {
			closeOut = &outs[i]
		}
	}
	if closeOut == nil || !near(closeOut.Price, 225) {
		t.Errorf("day close = %+v, want B's last price 225", closeOut)
	}
}

func TestPickWithoutAnyBarBeforeItStaysPending(t *testing.T) {
	const day = "2026-10-05"
	dir := t.TempDir()
	writeBars(t, dir, store.KindBars, minuteBars(day, stockA, func(k int) float64 { return 100 }))
	l := openLog(t)
	// B was never recorded.
	addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowPick, Signal: "pick", Instrument: stockB, RefTime: at(day, 9, 20), Direction: 1})
	res, err := Fill(bg, l, dir, day, day, time.Now())
	if err != nil || res.Filled != 0 || res.Pending != 3 {
		t.Errorf("Fill = %+v, %v", res, err)
	}
}

func sig(id int64, day, label string, dir int, traded bool, verdict string) decisionlog.ShadowSignal {
	kind := decisionlog.ShadowDecision
	if label == "pick" {
		kind = decisionlog.ShadowPick
	}
	action := strings.TrimPrefix(label, "decision:")
	return decisionlog.ShadowSignal{ID: id, Day: day, Kind: kind, Signal: label, Direction: dir, Action: action,
		Traded: traded, Verdict: verdict, RefTime: at(day, 10, 0)}
}

func out(id int64, h string, ret, idx float64) decisionlog.ShadowOutcome {
	return decisionlog.ShadowOutcome{SignalID: id, Horizon: h, ReturnPct: ret, IndexPct: &idx, UniversePct: new(float64)}
}

func rowOf(t *testing.T, r Report, group, horizon string) Row {
	t.Helper()
	for _, row := range r.Rows {
		if row.Group == group && row.Horizon == horizon {
			return row
		}
	}
	t.Fatalf("no row %q %s in %+v", group, horizon, r.Rows)
	return Row{}
}

func TestReportNetsTheBenchmarkAndReadsTheDirection(t *testing.T) {
	signals := []decisionlog.ShadowSignal{
		sig(1, "2026-10-05", "decision:BUY", 1, true, "approved"),
		sig(2, "2026-10-05", "decision:BUY", 1, false, "below_threshold"),
		sig(3, "2026-10-06", "decision:SELL", -1, false, "short_not_allowed"),
		sig(4, "2026-10-06", "decision:SELL", -1, false, "short_not_allowed"),
		sig(5, "2026-10-06", "decision:HOLD", 1, false, "hold"),
	}
	outcomes := []decisionlog.ShadowOutcome{
		out(1, Horizon60m, 1.5, 0.5),  // net +1.0
		out(2, Horizon60m, -0.2, 0.3), // net -0.5
		out(3, Horizon60m, -1.0, 0.0), // fell 1.0: a SELL is right, +1.0
		out(4, Horizon60m, 0.4, 0.2),  // net +0.2: a SELL is wrong, -0.2
		out(5, Horizon60m, 0.1, 0.1),  // net 0
		out(1, HorizonDayEnd, 2.0, 1.0),
	}
	rep, err := BuildReport(signals, outcomes, Options{From: "2026-10-05", To: "2026-10-06", ByVerdict: true, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Signals != 5 || rep.Days != 2 || rep.Benchmark != BenchmarkIndex {
		t.Errorf("header = %+v", rep)
	}
	buy := rowOf(t, rep, "decision:BUY", Horizon60m)
	if buy.N != 2 || buy.Days != 1 || !near(buy.MeanPct, 0.25) || !near(buy.HitRate, 0.5) || buy.HasCI {
		t.Errorf("BUY = %+v", buy)
	}
	if r := rowOf(t, rep, "decision:BUY traded", Horizon60m); r.N != 1 || !near(r.MeanPct, 1.0) || !near(r.HitRate, 1) {
		t.Errorf("BUY traded = %+v", r)
	}
	if r := rowOf(t, rep, "decision:BUY not traded", Horizon60m); r.N != 1 || !near(r.MeanPct, -0.5) || r.HitRate != 0 {
		t.Errorf("BUY not traded = %+v", r)
	}
	if r := rowOf(t, rep, "decision:BUY [below_threshold]", Horizon60m); r.N != 1 {
		t.Errorf("BUY by verdict = %+v", r)
	}
	sell := rowOf(t, rep, "decision:SELL", Horizon60m)
	if sell.N != 2 || !near(sell.MeanPct, 0.4) || !near(sell.HitRate, 0.5) {
		t.Errorf("SELL = %+v", sell)
	}
	// A HOLD has no traded split and no verdict group.
	for _, row := range rep.Rows {
		if strings.HasPrefix(row.Group, "decision:HOLD ") {
			t.Errorf("HOLD split: %+v", row)
		}
	}
	if h := rowOf(t, rep, "decision:HOLD", Horizon60m); h.N != 1 || h.HitRate != 0 {
		t.Errorf("HOLD = %+v", h)
	}
	// Outcomes of other horizons stay apart; the close is not awaited, the rest is.
	if r := rowOf(t, rep, "decision:BUY", HorizonDayEnd); r.N != 1 || !near(r.MeanPct, 1.0) {
		t.Errorf("BUY close = %+v", r)
	}
}

func TestReportBenchmarks(t *testing.T) {
	signals := []decisionlog.ShadowSignal{sig(1, "2026-10-05", "pick", 1, false, "")}
	idx, uni := 0.5, 0.25
	outcomes := []decisionlog.ShadowOutcome{{SignalID: 1, Horizon: Horizon15m, ReturnPct: 2, IndexPct: &idx, UniversePct: &uni}}
	for _, c := range []struct {
		bench string
		want  float64
	}{{BenchmarkIndex, 1.5}, {BenchmarkUniverse, 1.75}, {BenchmarkNone, 2}} {
		rep, err := BuildReport(signals, outcomes, Options{Benchmark: c.bench})
		if err != nil || len(rep.Rows) != 1 || !near(rep.Rows[0].MeanPct, c.want) {
			t.Errorf("%s: %+v, %v; want mean %v", c.bench, rep.Rows, err, c.want)
		}
	}
	// An outcome with no index return is left out of an index report, and counted.
	outcomes[0].IndexPct = nil
	rep, _ := BuildReport(signals, outcomes, Options{})
	if len(rep.Rows) != 0 || rep.Unbenchmarked != 1 {
		t.Errorf("no index: %+v", rep)
	}
	if _, err := BuildReport(signals, outcomes, Options{Benchmark: "nifty"}); err == nil {
		t.Error("an unknown benchmark was accepted")
	}
}

func TestReportCountsWhatIsStillAwaited(t *testing.T) {
	s := sig(1, "2026-10-05", "pick", 1, false, "")
	s.WindowEnd = at("2026-10-05", 15, 0)
	// 10:00: +15m, +60m, window end and the close all apply; one is filled.
	rep, err := BuildReport([]decisionlog.ShadowSignal{s}, []decisionlog.ShadowOutcome{out(1, Horizon15m, 1, 0)}, Options{})
	if err != nil || rep.Awaiting != 3 {
		t.Errorf("awaiting = %d, %v", rep.Awaiting, err)
	}
	var b strings.Builder
	if err := rep.Write(&b); err != nil || !strings.Contains(b.String(), "3 outcomes the recorded bars do not reach yet") {
		t.Errorf("text = %q, %v", b.String(), err)
	}
}

func TestDayBootstrapResamplesDays(t *testing.T) {
	// Five days whose outcomes average 1 each: whatever the days drawn, the mean is 1.
	flat := map[string][]float64{}
	for i, d := range []string{"a", "b", "c", "d", "e"} {
		flat[d] = []float64{1, 1, 1}[:1+i%3]
	}
	if lo, hi := dayBootstrap(flat, 500, 1); !near(lo, 1) || !near(hi, 1) {
		t.Errorf("flat days: CI = [%v, %v], want [1, 1]", lo, hi)
	}

	// Two days far apart: resampling gives the two extremes and the middle.
	two := map[string][]float64{"a": {0}, "b": {10}}
	lo, hi := dayBootstrap(two, 4000, 1)
	if !near(lo, 0) || !near(hi, 10) {
		t.Errorf("two days: CI = [%v, %v], want [0, 10]", lo, hi)
	}

	// Many outcomes in one day do not tighten the interval: the day is the unit.
	spread := map[string][]float64{"a": {1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, "b": {-1}, "c": {0}}
	lo, hi = dayBootstrap(spread, 4000, 1)
	if hi-lo < 1 {
		t.Errorf("CI [%v, %v] is too tight for three days that disagree", lo, hi)
	}

	// The same seed gives the same interval, whatever the map's order.
	for range 5 {
		l2, h2 := dayBootstrap(spread, 4000, 1)
		if l2 != lo || h2 != hi {
			t.Fatalf("not reproducible: [%v, %v] then [%v, %v]", lo, hi, l2, h2)
		}
	}
	many := map[string][]float64{}
	for i := range 12 {
		many[string(rune('a'+i))] = []float64{float64(i*i%7) - 3}
	}
	l1, h1 := dayBootstrap(many, 200, 1)
	if l3, h3 := dayBootstrap(many, 200, 2); l3 == l1 && h3 == h1 {
		t.Error("the seed has no effect")
	}
}

func TestReportIntervalCoversTheMean(t *testing.T) {
	var signals []decisionlog.ShadowSignal
	var outcomes []decisionlog.ShadowOutcome
	id := int64(0)
	for d := 1; d <= 8; d++ {
		day := time.Date(2026, 10, d, 0, 0, 0, 0, ist).Format(time.DateOnly)
		for k := range 4 {
			id++
			signals = append(signals, sig(id, day, "pick", 1, false, ""))
			outcomes = append(outcomes, out(id, Horizon60m, float64(d%4)-1.0+float64(k)*0.1, 0))
		}
	}
	rep, err := BuildReport(signals, outcomes, Options{Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	row := rowOf(t, rep, "pick", Horizon60m)
	if row.N != 32 || row.Days != 8 || !row.HasCI || row.CILow > row.MeanPct || row.CIHigh < row.MeanPct || row.CILow >= row.CIHigh {
		t.Errorf("row = %+v", row)
	}
}

func TestEncodePickKeepsTheMostMaterialNews(t *testing.T) {
	e := watchlist.Entry{Rank: 3, Key: "NSE:AAA", Sector: "IT", Score: 0.42,
		Signals: map[string]float64{"news": 0.8, "gap": 0.1},
		News: []watchlist.NewsRef{
			{Title: "minor", Impact: 1, Material: 0.2, Direction: 0.9},
			{Title: "results", Impact: 3, Material: 0.9, Direction: 0.7},
		}}
	var d PickDetail
	if err := json.Unmarshal(EncodePick(e), &d); err != nil {
		t.Fatal(err)
	}
	if d.Rank != 3 || d.Score != 0.42 || d.Signals["news"] != 0.8 || d.News == nil || d.News.Title != "results" || d.News.Direction != 0.7 {
		t.Errorf("detail = %+v", d)
	}
	if d := EncodePick(watchlist.Entry{Key: "NSE:BBB"}); d == nil {
		t.Error("an entry with no news has no detail")
	}
	if EncodePick(watchlist.Entry{Score: math.NaN()}) != nil {
		t.Error("a NaN score was encoded")
	}
}

func TestFillReadsTheLogFromAFile(t *testing.T) {
	// The same path the command takes: a log on disk, bars on disk.
	const day = "2026-10-05"
	dir := t.TempDir()
	writeBars(t, dir, store.KindBars, minuteBars(day, stockA, func(k int) float64 { return 100 + float64(k) }))
	path := filepath.Join(t.TempDir(), "decisions.db")
	l, err := decisionlog.Open(path, decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.StartRun(bg, decisionlog.Run{ID: "r1", Mode: "paper", Decider: "jev", Source: "kite", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	addSignal(t, l, decisionlog.ShadowSignal{Kind: decisionlog.ShadowPick, Signal: "pick", Instrument: stockA, RefTime: at(day, 9, 20), Direction: 1})
	l.Close()
	l, err = decisionlog.Open(path, decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if res, err := Fill(bg, l, dir, day, day, time.Now()); err != nil || res.Filled != 3 {
		t.Errorf("Fill = %+v, %v", res, err)
	}
}
