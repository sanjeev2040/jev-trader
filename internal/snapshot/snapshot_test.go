package snapshot

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	ist     = time.FixedZone("IST", 5*3600+1800)
	infy    = domain.Instrument{Token: 408065, Exchange: "NSE", Symbol: "INFY"}
	nifty   = domain.Instrument{Token: 256265, Exchange: "NSE", Symbol: "NIFTY 50"}
	testCfg = Config{SizeThresholds: [2]float64{50_000, 200_000}}
)

// session returns the open and close of the session on the given day.
func session(day time.Time) (open, close time.Time) {
	y, m, d := day.Date()
	return time.Date(y, m, d, 9, 15, 0, 0, ist), time.Date(y, m, d, 15, 30, 0, 0, ist)
}

// fixtureBars returns n 1-minute bars from open, with prices scaled by scale.
// The close path is deterministic, drifts up and moves both ways.
func fixtureBars(inst domain.Instrument, open time.Time, n int, scale float64) []domain.Bar {
	bars := make([]domain.Bar, n)
	px := 100.0
	for i := range bars {
		o := px
		px += float64((i*7)%5-2)*0.1 + 0.03
		c := px
		start := open.Add(time.Duration(i) * time.Minute)
		bars[i] = domain.Bar{
			Instrument: inst,
			Interval:   time.Minute,
			Start:      start,
			End:        start.Add(time.Minute),
			Open:       o * scale,
			High:       (max(o, c) + 0.05) * scale,
			Low:        (min(o, c) - 0.05) * scale,
			Close:      c * scale,
			Volume:     int64(1000 + (i%4)*250),
		}
	}
	return bars
}

func fixtureInput(day time.Time, n int, scale float64) Input {
	open, close := session(day)
	bars := fixtureBars(infy, open, n, scale)
	return Input{
		Alias:        "I01",
		Now:          bars[n-1].End,
		SessionOpen:  open,
		SessionClose: close,
		Bars:         bars,
		IndexBars:    fixtureBars(nifty, open, n, 220*scale),
		Position:     domain.Position{Instrument: infy, Quantity: 100, AvgPrice: 99.5 * scale},
	}
}

var day1 = time.Date(2026, 3, 2, 0, 0, 0, 0, ist)

func mustEncode(t *testing.T, in Input) []byte {
	t.Helper()
	s, err := Build(testCfg, in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	b, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
}

func TestGolden(t *testing.T) {
	got := mustEncode(t, fixtureInput(day1, 40, 1))
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')

	path := filepath.Join("testdata", "snapshot_v"+strconv.Itoa(SchemaVersion)+".golden.json")
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Errorf("snapshot JSON changed; bump SchemaVersion if the schema changed, then run with -update\ngot:\n%s\nwant:\n%s", pretty.Bytes(), want)
	}
}

func TestNoIdentifyingData(t *testing.T) {
	in := fixtureInput(day1, 40, 1)
	got := string(mustEncode(t, in))

	forbidden := []string{
		"INFY", "NIFTY", "NSE", "408065", "256265",
		"2026", "Mar", "Monday", "09:15", "IST", "+05:30",
	}
	for _, f := range forbidden {
		if strings.Contains(got, f) {
			t.Errorf("snapshot contains %q: %s", f, got)
		}
	}
	// No absolute price level of the last bars or the position.
	last := in.Bars[len(in.Bars)-1]
	for _, px := range []float64{last.Close, last.Open, last.High, last.Low, in.Position.AvgPrice, in.IndexBars[len(in.IndexBars)-1].Close} {
		if s := strconv.FormatFloat(px, 'f', 2, 64); strings.Contains(got, s) {
			t.Errorf("snapshot contains price %s: %s", s, got)
		}
	}
}

// The same intraday pattern on another date, at another price level, must
// produce the identical snapshot: the state carries no date and no scale.
func TestInvariantToDateAndPriceLevel(t *testing.T) {
	// Size buckets are notional, so give both the same notional exposure.
	// A power-of-two scale keeps the price ratios exact in floating point.
	a := fixtureInput(day1, 40, 1)
	b := fixtureInput(time.Date(2019, 11, 15, 0, 0, 0, 0, ist), 40, 4)
	b.Position.Quantity = 25
	base, other := mustEncode(t, a), mustEncode(t, b)
	if !bytes.Equal(base, other) {
		t.Errorf("snapshot depends on date or price level:\n%s\n%s", base, other)
	}
}

func TestDeterministic(t *testing.T) {
	in := fixtureInput(day1, 40, 1)
	if a, b := mustEncode(t, in), mustEncode(t, in); !bytes.Equal(a, b) {
		t.Errorf("Build is not deterministic:\n%s\n%s", a, b)
	}
}

func TestShortHistoryGivesNulls(t *testing.T) {
	in := fixtureInput(day1, 3, 1)
	in.IndexBars = nil
	s, err := Build(testCfg, in)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Features.ReturnsPct
	if r.Bars1 == nil || r.Bars5 != nil || r.Bars15 != nil || r.Bars30 != nil {
		t.Errorf("returns = %+v, want only bars_1 set", r)
	}
	if s.Index.Available || s.Index.ReturnsPct.Bars1 != nil {
		t.Errorf("index = %+v, want unavailable", s.Index)
	}
	if s.Session.MinutesSinceOpen != 3 || s.Session.MinutesToClose != 372 {
		t.Errorf("session = %+v, want {3 372}", s.Session)
	}
}

func TestFeatures(t *testing.T) {
	open, close := session(day1)
	bar := func(i int, o, h, l, c float64, v int64) domain.Bar {
		start := open.Add(time.Duration(i) * time.Minute)
		return domain.Bar{Instrument: infy, Interval: time.Minute, Start: start, End: start.Add(time.Minute),
			Open: o, High: h, Low: l, Close: c, Volume: v}
	}
	bars := []domain.Bar{
		bar(0, 100, 101, 99, 100, 100),
		bar(1, 100, 104, 100, 104, 300),
	}
	s, err := Build(testCfg, Input{Alias: "I01", Now: bars[1].End, SessionOpen: open, SessionClose: close, Bars: bars})
	if err != nil {
		t.Fatal(err)
	}
	f := s.Features
	// VWAP = (100*100 + 300*(104+100+104)/3) / 400 = 102.0
	checks := []struct {
		name      string
		got, want float64
	}{
		{"bars_1", *f.ReturnsPct.Bars1, 4},
		{"session_return_pct", f.SessionReturnPct, 4},
		{"bar_range_pct", f.BarRangePct, 3.8462},
		{"session_range_pos", *f.SessionRangePos, 1},
		{"volume_rel", *f.VolumeRel, 1.5},
		{"vwap_dist_pct", *f.VWAPDistPct, 1.9608},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if s.Position.Side != SideFlat || s.Position.SizeBucket != SizeNone || s.Position.UnrealizedPnLPct != nil {
		t.Errorf("position = %+v, want flat", s.Position)
	}
}

func TestPosition(t *testing.T) {
	tests := []struct {
		name       string
		qty        int64
		avg        float64
		wantSide   string
		wantBucket string
		wantPnL    float64
	}{
		{"long small", 100, 99.5, SideLong, SizeS, 0.5025},
		{"long medium", 1000, 99.5, SideLong, SizeM, 0.5025},
		{"short large", -5000, 101, SideShort, SizeL, 0.9901},
		{"short at threshold", -500, 100, SideShort, SizeS, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := fixtureInput(day1, 40, 1)
			in.Bars[len(in.Bars)-1].Close = 100
			in.Position = domain.Position{Instrument: infy, Quantity: tt.qty, AvgPrice: tt.avg}
			s, err := Build(testCfg, in)
			if err != nil {
				t.Fatal(err)
			}
			p := s.Position
			if p.Side != tt.wantSide || p.SizeBucket != tt.wantBucket || p.UnrealizedPnLPct == nil || *p.UnrealizedPnLPct != tt.wantPnL {
				t.Errorf("position = {%s %s %v}, want {%s %s %v}", p.Side, p.SizeBucket, deref(p.UnrealizedPnLPct), tt.wantSide, tt.wantBucket, tt.wantPnL)
			}
		})
	}
}

func TestBuildRejects(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		mutate func(*Input)
		want   string
	}{
		{"bar closes after now", testCfg, func(in *Input) { in.Now = in.Now.Add(-time.Second) }, "look-ahead"},
		{"index bar closes after now", testCfg, func(in *Input) {
			in.IndexBars = append(in.IndexBars, fixtureBars(nifty, in.Now, 1, 220)...)
		}, "look-ahead"},
		{"alias is symbol", testCfg, func(in *Input) { in.Alias = "infy" }, "symbol"},
		{"alias has spaces", testCfg, func(in *Input) { in.Alias = "my stock" }, "alias"},
		{"empty alias", testCfg, func(in *Input) { in.Alias = "" }, "alias"},
		{"no bars", testCfg, func(in *Input) { in.Bars = nil }, "no bars"},
		{"mixed instruments", testCfg, func(in *Input) { in.Bars[3].Instrument = nifty }, "different instrument"},
		{"out of order", testCfg, func(in *Input) { in.Bars[2], in.Bars[3] = in.Bars[3], in.Bars[2] }, "out of order"},
		{"bar before open", testCfg, func(in *Input) { in.SessionOpen = in.SessionOpen.Add(time.Minute) }, "before session open"},
		{"zero price", testCfg, func(in *Input) { in.Bars[5].Low = 0 }, "invalid"},
		{"position other instrument", testCfg, func(in *Input) { in.Position.Instrument = nifty }, "position"},
		{"position without price", testCfg, func(in *Input) { in.Position.AvgPrice = 0 }, "average price"},
		{"bad session", testCfg, func(in *Input) { in.SessionClose = in.SessionOpen }, "session"},
		{"bad thresholds", Config{SizeThresholds: [2]float64{10, 5}}, func(*Input) {}, "thresholds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := fixtureInput(day1, 40, 1)
			tt.mutate(&in)
			_, err := Build(tt.cfg, in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestAliases(t *testing.T) {
	a := NewAliases()
	if got := a.Alias(infy); got != "I01" {
		t.Errorf("first alias = %q", got)
	}
	if got := a.Alias(nifty); got != "I02" {
		t.Errorf("second alias = %q", got)
	}
	if got := a.Alias(infy); got != "I01" {
		t.Errorf("repeat alias = %q", got)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
