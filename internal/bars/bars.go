// Package bars aggregates ticks into OHLCV bars.
//
// The Builder is pure: it has no goroutines, timers or I/O, and time only moves
// when the caller passes a tick (event time) or calls Advance. It therefore
// behaves identically under a wall clock and a simulated replay clock.
package bars

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Bar boundaries are aligned with time.Truncate, which counts from the UTC
// epoch. IST is UTC+05:30, so an interval that divides 30 minutes also starts
// on IST boundaries (09:15, 09:20, ...). Longer intervals would not.
const maxInterval = 30 * time.Minute

// ist is the exchange timezone. It is used only to detect a new trading day
// when the cumulative day volume resets.
var ist = time.FixedZone("IST", 5*3600+30*60)

// Config controls a Builder. The zero value means 1m base bars, 5m derived
// bars and a 2s close grace.
type Config struct {
	// Base is the interval of bars built directly from ticks. Default 1m.
	Base time.Duration
	// Derived intervals are built from closed base bars. Each must be a
	// multiple of Base. Default [5m]; set to an empty non-nil slice for none.
	Derived []time.Duration
	// Grace is how long after a bar's end Advance waits before closing it,
	// so ticks stamped just before the boundary can still arrive. Default 2s.
	Grace time.Duration
}

// Stats counts ticks the Builder rejected.
type Stats struct {
	Late    uint64 // ticks older than a bar the instrument already closed or opened
	Invalid uint64 // ticks with no event time or a non-positive or non-finite price
}

// Builder aggregates ticks for any number of instruments. It is not safe for
// concurrent use; the engine loop owns it.
type Builder struct {
	base    time.Duration
	derived []time.Duration
	grace   time.Duration
	inst    map[uint32]*instrument
	stats   Stats
}

type instrument struct {
	base          *domain.Bar   // open base bar, nil if none
	derived       []*domain.Bar // open derived bar per Config.Derived, nil if none
	closedThrough time.Time     // bars ending at or before this are final

	haveVolume bool
	volume     int64  // last accepted cumulative day volume
	volumeDay  string // IST date of that observation
}

// New returns a Builder for cfg.
func New(cfg Config) (*Builder, error) {
	if cfg.Base == 0 {
		cfg.Base = time.Minute
	}
	if cfg.Derived == nil {
		cfg.Derived = []time.Duration{5 * time.Minute}
	}
	if cfg.Grace == 0 {
		cfg.Grace = 2 * time.Second
	}
	if err := checkInterval(cfg.Base); err != nil {
		return nil, fmt.Errorf("bars: base %w", err)
	}
	if cfg.Grace < 0 {
		return nil, fmt.Errorf("bars: negative grace %v", cfg.Grace)
	}
	for i, d := range cfg.Derived {
		if err := checkInterval(d); err != nil {
			return nil, fmt.Errorf("bars: derived %w", err)
		}
		if d <= cfg.Base || d%cfg.Base != 0 {
			return nil, fmt.Errorf("bars: derived interval %v is not a multiple of base %v", d, cfg.Base)
		}
		if slices.Contains(cfg.Derived[:i], d) {
			return nil, fmt.Errorf("bars: duplicate derived interval %v", d)
		}
	}
	return &Builder{
		base:    cfg.Base,
		derived: slices.Clone(cfg.Derived),
		grace:   cfg.Grace,
		inst:    make(map[uint32]*instrument),
	}, nil
}

func checkInterval(d time.Duration) error {
	if d <= 0 || d > maxInterval || maxInterval%d != 0 {
		return fmt.Errorf("interval %v must be positive and divide %v", d, maxInterval)
	}
	return nil
}

// Stats returns the rejection counters.
func (b *Builder) Stats() Stats { return b.stats }

// Add feeds one tick and returns the bars it closed, ordered by end time,
// then interval, then instrument token.
//
// A bar closes when an instrument's tick event time reaches the bar's end.
// Ticks that belong to a bar already closed, or that are older than the
// instrument's open bar, are counted as late and dropped. Their volume is not
// lost: the next accepted tick's cumulative volume still includes it.
func (b *Builder) Add(t domain.Tick) []domain.Bar {
	if t.ExchangeTime.IsZero() || !(t.LastPrice > 0) || math.IsInf(t.LastPrice, 0) {
		b.stats.Invalid++
		return nil
	}
	st := b.inst[t.InstrumentToken]
	if st == nil {
		st = &instrument{derived: make([]*domain.Bar, len(b.derived))}
		b.inst[t.InstrumentToken] = st
	}
	start := t.ExchangeTime.Truncate(b.base)
	if !start.Add(b.base).After(st.closedThrough) || (st.base != nil && start.Before(st.base.Start)) {
		b.stats.Late++
		return nil
	}

	var out []domain.Bar
	b.closeUpTo(st, t.ExchangeTime, &out)

	if st.base == nil {
		st.base = &domain.Bar{
			Instrument: instrumentOf(t),
			Interval:   b.base,
			Start:      start,
			End:        start.Add(b.base),
			Open:       t.LastPrice,
			High:       t.LastPrice,
			Low:        t.LastPrice,
		}
	}
	bar := st.base
	bar.High = max(bar.High, t.LastPrice)
	bar.Low = min(bar.Low, t.LastPrice)
	bar.Close = t.LastPrice
	bar.Volume += st.volumeDelta(t)

	Sort(out)
	return out
}

// Advance closes every bar whose end is at least the grace period before now,
// and returns them in the same order as Add. Call it from the clock so bars of
// illiquid instruments still close on time. Passing a time far in the future
// flushes every open bar.
func (b *Builder) Advance(now time.Time) []domain.Bar {
	cutoff := now.Add(-b.grace)
	var out []domain.Bar
	for _, st := range b.inst {
		b.closeUpTo(st, cutoff, &out)
	}
	Sort(out)
	return out
}

// closeUpTo closes the instrument's open bars that end at or before ts and
// marks every bar ending by ts final, so a later tick cannot reopen one.
func (b *Builder) closeUpTo(st *instrument, ts time.Time, out *[]domain.Bar) {
	if ts.After(st.closedThrough) {
		st.closedThrough = ts
	}
	if st.base != nil && !ts.Before(end(*st.base)) {
		closed := *st.base
		st.base = nil
		*out = append(*out, closed)
		b.feedDerived(st, closed, out)
	}
	for i, d := range st.derived {
		if d != nil && !ts.Before(end(*d)) {
			*out = append(*out, *d)
			st.derived[i] = nil
		}
	}
}

// feedDerived merges a closed base bar into each derived bar, closing a
// derived bar once the base bar that completes it has been merged.
func (b *Builder) feedDerived(st *instrument, bar domain.Bar, out *[]domain.Bar) {
	for i, interval := range b.derived {
		start := bar.Start.Truncate(interval)
		if d := st.derived[i]; d != nil && !start.Before(end(*d)) {
			*out = append(*out, *d)
			st.derived[i] = nil
		}
		if st.derived[i] == nil {
			st.derived[i] = &domain.Bar{
				Instrument: bar.Instrument,
				Interval:   interval,
				Start:      start,
				End:        start.Add(interval),
				Open:       bar.Open,
				High:       bar.High,
				Low:        bar.Low,
			}
		}
		merge(st.derived[i], bar)
		if d := st.derived[i]; !end(bar).Before(end(*d)) {
			*out = append(*out, *d)
			st.derived[i] = nil
		}
	}
}

// volumeDelta returns the volume traded since the previous accepted tick,
// derived from the cumulative day volume rather than from tick quantities.
//
// The first observation of an instrument only sets the baseline. A lower
// observation on a later IST date is a new day, so the whole value counts; a
// lower value on the same date is stale data and counts nothing.
func (st *instrument) volumeDelta(t domain.Tick) int64 {
	day := t.ExchangeTime.In(ist).Format(time.DateOnly)
	var delta int64
	switch {
	case !st.haveVolume:
	case day > st.volumeDay:
		delta = t.Volume
	case t.Volume >= st.volume:
		delta = t.Volume - st.volume
	default:
		return 0 // stale; keep the higher baseline
	}
	st.haveVolume, st.volume, st.volumeDay = true, t.Volume, day
	return delta
}

// Aggregate resamples closed bars into bars of interval. Input bars may be in
// any order and for any instruments, but must all share one interval that
// divides interval. The result is ordered like Builder output. Partial
// windows (for example a trailing window with missing minutes) are returned as
// they are; minutes with no input bar are not filled in.
func Aggregate(in []domain.Bar, interval time.Duration) ([]domain.Bar, error) {
	if err := checkInterval(interval); err != nil {
		return nil, fmt.Errorf("bars: %w", err)
	}
	if len(in) == 0 {
		return nil, nil
	}
	src := in[0].Interval
	for _, bar := range in {
		if bar.Interval != src {
			return nil, fmt.Errorf("bars: mixed input intervals %v and %v", src, bar.Interval)
		}
	}
	if src <= 0 || interval%src != 0 {
		return nil, fmt.Errorf("bars: input interval %v does not divide %v", src, interval)
	}
	sorted := slices.Clone(in)
	slices.SortFunc(sorted, func(a, b domain.Bar) int {
		if a.Instrument.Token != b.Instrument.Token {
			return cmp.Compare(a.Instrument.Token, b.Instrument.Token)
		}
		return a.Start.Compare(b.Start)
	})
	var out []domain.Bar
	for _, bar := range sorted {
		start := bar.Start.Truncate(interval)
		if n := len(out); n > 0 && out[n-1].Instrument.Token == bar.Instrument.Token && out[n-1].Start.Equal(start) {
			merge(&out[n-1], bar)
			continue
		}
		agg := domain.Bar{
			Instrument: bar.Instrument,
			Interval:   interval,
			Start:      start,
			End:        start.Add(interval),
			Open:       bar.Open,
			High:       bar.High,
			Low:        bar.Low,
		}
		merge(&agg, bar)
		out = append(out, agg)
	}
	Sort(out)
	return out, nil
}

// merge folds a later bar into agg. agg.Open must already be set.
func merge(agg *domain.Bar, bar domain.Bar) {
	agg.High = max(agg.High, bar.High)
	agg.Low = min(agg.Low, bar.Low)
	agg.Close = bar.Close
	agg.Volume += bar.Volume
}

func end(bar domain.Bar) time.Time { return bar.Start.Add(bar.Interval) }

// instrumentOf identifies a tick's instrument. The symbol is "EXCHANGE:SYMBOL"
// and empty when the token is not in the watchlist.
func instrumentOf(t domain.Tick) domain.Instrument {
	in := domain.Instrument{Token: t.InstrumentToken}
	if ex, sym, ok := strings.Cut(t.Symbol, ":"); ok {
		in.Exchange, in.Symbol = ex, sym
	} else {
		in.Symbol = t.Symbol
	}
	return in
}

// Sort orders bars by end time, then interval, then instrument token: the
// order Builder emits, deterministic regardless of map iteration order.
func Sort(bars []domain.Bar) {
	slices.SortFunc(bars, func(a, b domain.Bar) int {
		if c := end(a).Compare(end(b)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Interval, b.Interval); c != 0 {
			return c
		}
		return cmp.Compare(a.Instrument.Token, b.Instrument.Token)
	})
}
