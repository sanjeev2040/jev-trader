package shadow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/store"
)

// The horizons an outcome is measured at, counted from the signal's time.
const (
	Horizon15m    = "+15m"
	Horizon60m    = "+60m"
	HorizonWindow = "window_end"
	HorizonDayEnd = "day_close"
)

// Horizons are the horizons in report order.
var Horizons = []string{Horizon15m, Horizon60m, HorizonWindow, HorizonDayEnd}

// dayCloseOffset is the NSE close as an offset from IST midnight.
const dayCloseOffset = 15*time.Hour + 30*time.Minute

var ist = time.FixedZone("IST", 5*3600+30*60)

func istMidnight(t time.Time) time.Time {
	y, m, d := t.In(ist).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, ist)
}

// target returns the time a horizon of a signal falls on, and false when the
// horizon does not apply: a window end the signal has none of or that is not
// after it, or a target past the day's close, which no bar can ever reach.
func target(s decisionlog.ShadowSignal, horizon string) (time.Time, bool) {
	var t time.Time
	switch horizon {
	case Horizon15m:
		t = s.RefTime.Add(15 * time.Minute)
	case Horizon60m:
		t = s.RefTime.Add(60 * time.Minute)
	case HorizonWindow:
		t = s.WindowEnd
	case HorizonDayEnd:
		t = istMidnight(s.RefTime).Add(dayCloseOffset)
	}
	if t.IsZero() || !t.After(s.RefTime) || t.After(istMidnight(s.RefTime).Add(dayCloseOffset)) {
		return time.Time{}, false
	}
	return t, true
}

// FillResult counts what Fill did.
type FillResult struct {
	Signals int // signals in range
	Filled  int // outcomes written by this call
	// Pending are the outcomes still missing that apply: the recorded bars do
	// not reach the horizon yet, or the instrument has no bar at or before the
	// signal. A later Fill, after more data is recorded or backfilled, takes
	// them.
	Pending int
}

// Fill writes the missing outcomes of the shadow signals of the IST days
// from..to (YYYY-MM-DD, inclusive) from the bars recorded under dataDir. It
// is safe to repeat: an outcome already in the log is kept, and one the bars
// cannot answer yet is left for a later call. now stamps the new rows.
func Fill(ctx context.Context, log *decisionlog.Log, dataDir, from, to string, now time.Time) (FillResult, error) {
	signals, err := log.ShadowSignals(ctx, from, to)
	if err != nil {
		return FillResult{}, err
	}
	have, err := log.ShadowOutcomes(ctx, from, to)
	if err != nil {
		return FillResult{}, err
	}
	done := make(map[outcomeKey]bool, len(have))
	for _, o := range have {
		done[outcomeKey{o.SignalID, o.Horizon}] = true
	}
	byDay := map[string][]decisionlog.ShadowSignal{}
	var days []string
	for _, s := range signals {
		if _, ok := byDay[s.Day]; !ok {
			days = append(days, s.Day)
		}
		byDay[s.Day] = append(byDay[s.Day], s)
	}
	res := FillResult{Signals: len(signals)}
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var open []decisionlog.ShadowSignal // signals with an outcome still to write
		for _, s := range byDay[day] {
			for _, h := range Horizons {
				if _, ok := target(s, h); ok && !done[outcomeKey{s.ID, h}] {
					open = append(open, s)
					break
				}
			}
		}
		if len(open) == 0 {
			continue
		}
		tape, err := loadTape(dataDir, day)
		if err != nil {
			return res, err
		}
		universe := universeTokens(byDay[day])
		for _, s := range open {
			for _, h := range Horizons {
				tgt, ok := target(s, h)
				if !ok || done[outcomeKey{s.ID, h}] {
					continue
				}
				o, ok := tape.outcome(s, h, tgt, universe, now)
				if !ok {
					res.Pending++
					continue
				}
				if err := log.AppendShadowOutcome(ctx, o); err != nil {
					return res, err
				}
				res.Filled++
			}
		}
	}
	return res, nil
}

type outcomeKey struct {
	id      int64
	horizon string
}

// universeTokens are the stocks the day's signals were about, with the
// indices left out: the day's screened universe, as far as it was recorded.
func universeTokens(signals []decisionlog.ShadowSignal) []uint32 {
	index := map[uint32]bool{}
	for _, s := range signals {
		index[s.IndexToken] = true
	}
	seen := map[uint32]bool{}
	var out []uint32
	for _, s := range signals {
		if t := s.Instrument.Token; !index[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// tape is a day's recorded one-minute bars by instrument, in end order.
type tape struct {
	bars map[uint32][]domain.Bar
	// reach is the end of the last bar of any instrument: how far the day was
	// observed. An illiquid stock can go quiet before it, and keeps its last
	// price until then.
	reach time.Time
}

// loadTape reads the one-minute bars of the IST day. Bars recorded live win
// over backfilled ones for the same instrument and minute.
func loadTape(dataDir, day string) (*tape, error) {
	d, err := time.ParseInLocation(time.DateOnly, day, ist)
	if err != nil {
		return nil, fmt.Errorf("shadow: day %q: %w", day, err)
	}
	q := store.BarQuery{Query: store.Query{From: d, To: d.Add(24 * time.Hour)}, Interval: time.Minute}
	t := &tape{bars: map[uint32][]domain.Bar{}}
	type key struct {
		token uint32
		end   int64
	}
	seen := map[key]bool{}
	for _, kind := range []string{store.KindBars, store.KindBackfill} {
		bs, err := store.ReadBars(dataDir, kind, q)
		if err != nil {
			return nil, fmt.Errorf("shadow: read %s bars of %s: %w", kind, day, err)
		}
		for _, b := range bs {
			k := key{b.Instrument.Token, b.End.UnixNano()}
			if seen[k] {
				continue
			}
			seen[k] = true
			t.bars[b.Instrument.Token] = append(t.bars[b.Instrument.Token], b)
			if b.End.After(t.reach) {
				t.reach = b.End
			}
		}
	}
	for _, bs := range t.bars {
		slices.SortFunc(bs, func(a, b domain.Bar) int { return a.End.Compare(b.End) })
	}
	return t, nil
}

// priceAt is the close of the last bar of token that ended at or before at.
func (t *tape) priceAt(token uint32, at time.Time) (float64, bool) {
	bs := t.bars[token]
	i, found := slices.BinarySearchFunc(bs, at, func(b domain.Bar, at time.Time) int { return b.End.Compare(at) })
	if !found {
		i-- // the first bar ending after at is not it: the one before is
	}
	if i < 0 || bs[i].Close <= 0 {
		return 0, false
	}
	return bs[i].Close, true
}

// move is the percentage move of token between two times.
func (t *tape) move(token uint32, from, to time.Time) (float64, bool) {
	a, ok := t.priceAt(token, from)
	if !ok {
		return 0, false
	}
	b, ok := t.priceAt(token, to)
	if !ok {
		return 0, false
	}
	return (b - a) / a * 100, true
}

// outcome measures one horizon of one signal. It is false when the recorded
// bars do not reach tgt, or the instrument has no bar at or before the signal.
func (t *tape) outcome(s decisionlog.ShadowSignal, horizon string, tgt time.Time, universe []uint32, now time.Time) (decisionlog.ShadowOutcome, bool) {
	if t.reach.Before(tgt) {
		return decisionlog.ShadowOutcome{}, false
	}
	token := s.Instrument.Token
	ref := s.RefPrice
	if ref <= 0 {
		p, ok := t.priceAt(token, s.RefTime)
		if !ok {
			return decisionlog.ShadowOutcome{}, false
		}
		ref = p
	}
	price, ok := t.priceAt(token, tgt)
	if !ok {
		return decisionlog.ShadowOutcome{}, false
	}
	o := decisionlog.ShadowOutcome{
		SignalID: s.ID, Horizon: horizon, Target: tgt, RefPrice: ref, Price: price,
		ReturnPct: (price - ref) / ref * 100, FilledAt: now,
	}
	if s.IndexToken != 0 {
		if m, ok := t.move(s.IndexToken, s.RefTime, tgt); ok {
			o.IndexPct = &m
		}
	}
	var sum float64
	for _, u := range universe {
		if m, ok := t.move(u, s.RefTime, tgt); ok {
			sum += m
			o.UniverseNames++
		}
	}
	if o.UniverseNames > 0 {
		m := sum / float64(o.UniverseNames)
		o.UniversePct = &m
	}
	return o, true
}
