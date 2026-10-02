// Package snapshot defines the versioned market state sent to Jev as the
// `state` field of a decision request, and builds it from closed bars, the
// open position and the session time.
//
// The schema is leakage-safe for backtests: it carries no calendar date,
// weekday, time of day, ticker symbol or absolute price level. Every value is
// scale-free (percentages and ratios), the session clock is relative to the
// open, and the instrument is named by an opaque per-run alias. Live and
// replay use the identical schema.
//
// Any change to the JSON shape or to the meaning of a field must bump
// SchemaVersion and update testdata/snapshot_v<N>.golden.json.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// SchemaVersion is the version of the Snapshot JSON schema.
const SchemaVersion = 1

// Lookbacks are the return horizons, in bars, reported in Returns.
var Lookbacks = [...]int{1, 5, 15, 30}

// Position sides.
const (
	SideFlat  = "FLAT"
	SideLong  = "LONG"
	SideShort = "SHORT"
)

// Position size buckets. They match the size answers Jev chooses from.
const (
	SizeNone = "NONE"
	SizeS    = "S"
	SizeM    = "M"
	SizeL    = "L"
)

// Snapshot is the state sent to Jev. Optional values are pointers and encode
// as null when there is not enough history to compute them.
type Snapshot struct {
	SchemaVersion int      `json:"schema_version"`
	Instrument    string   `json:"instrument"` // opaque per-run alias, never the symbol
	Session       Session  `json:"session"`
	Features      Features `json:"features"`
	Index         Index    `json:"index"`
	Position      Position `json:"position"`
}

// Session is the session clock, relative to the open.
type Session struct {
	MinutesSinceOpen int `json:"minutes_since_open"`
	MinutesToClose   int `json:"minutes_to_close"`
}

// Returns are percentage close-to-close returns over the Lookbacks.
type Returns struct {
	Bars1  *float64 `json:"bars_1"`
	Bars5  *float64 `json:"bars_5"`
	Bars15 *float64 `json:"bars_15"`
	Bars30 *float64 `json:"bars_30"`
}

// Features are the instrument's normalized, scale-free features.
type Features struct {
	ReturnsPct       Returns  `json:"returns_pct"`
	SessionReturnPct float64  `json:"session_return_pct"` // last close vs first session open
	BarRangePct      float64  `json:"bar_range_pct"`      // (high-low)/close of the last bar
	SessionRangePos  *float64 `json:"session_range_pos"`  // last close within session low..high, 0..1
	VolumeRel        *float64 `json:"volume_rel"`         // last bar volume / session mean bar volume
	VWAPDistPct      *float64 `json:"vwap_dist_pct"`      // last close vs session VWAP
}

// Index is the market context from the NIFTY 50 index. Available is false
// when no index bars were supplied; the other fields are then zero or null.
type Index struct {
	Available        bool    `json:"available"`
	ReturnsPct       Returns `json:"returns_pct"`
	SessionReturnPct float64 `json:"session_return_pct"`
}

// Position is the open position, described without absolute size or price.
type Position struct {
	Side             string   `json:"side"`
	SizeBucket       string   `json:"size_bucket"`
	UnrealizedPnLPct *float64 `json:"unrealized_pnl_pct"` // null when flat
}

// Config holds the builder's tunables.
type Config struct {
	// SizeThresholds are the upper notional bounds, in rupees, of the S and
	// M buckets. A position above SizeThresholds[1] is L.
	SizeThresholds [2]float64
}

// Validate reports whether c is usable.
func (c Config) Validate() error {
	if !(c.SizeThresholds[0] > 0 && c.SizeThresholds[1] > c.SizeThresholds[0]) {
		return fmt.Errorf("snapshot: size thresholds must satisfy 0 < S < M, got %v", c.SizeThresholds)
	}
	return nil
}

// Input is everything Build reads. All bars must be closed by Now.
type Input struct {
	Alias        string          // opaque per-run alias of the instrument
	Now          time.Time       // decision time, normally the triggering bar's close
	SessionOpen  time.Time       // today's session open
	SessionClose time.Time       // today's session close
	Bars         []domain.Bar    // the instrument's closed bars this session, oldest first
	IndexBars    []domain.Bar    // NIFTY 50 closed bars this session, oldest first; may be empty
	Position     domain.Position // open position; zero Quantity means flat
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Build returns the snapshot for in. It is a pure function of its arguments.
// It rejects input that would leak the future (a bar not closed by Now) or
// the instrument's identity (an alias equal to its symbol).
func Build(cfg Config, in Input) (Snapshot, error) {
	if err := cfg.Validate(); err != nil {
		return Snapshot{}, err
	}
	if err := validateAlias(in.Alias, in.Bars); err != nil {
		return Snapshot{}, err
	}
	if !in.SessionOpen.Before(in.SessionClose) {
		return Snapshot{}, errors.New("snapshot: session open must be before session close")
	}
	if in.Now.Before(in.SessionOpen) {
		return Snapshot{}, errors.New("snapshot: now is before session open")
	}
	if len(in.Bars) == 0 {
		return Snapshot{}, errors.New("snapshot: no bars")
	}
	if err := validateBars("bars", in.Bars, in.Now, in.SessionOpen); err != nil {
		return Snapshot{}, err
	}
	if err := validateBars("index bars", in.IndexBars, in.Now, in.SessionOpen); err != nil {
		return Snapshot{}, err
	}
	pos, err := buildPosition(cfg, in.Position, in.Bars)
	if err != nil {
		return Snapshot{}, err
	}

	s := Snapshot{
		SchemaVersion: SchemaVersion,
		Instrument:    in.Alias,
		Session: Session{
			MinutesSinceOpen: int(in.Now.Sub(in.SessionOpen) / time.Minute),
			MinutesToClose:   max(0, int(in.SessionClose.Sub(in.Now)/time.Minute)),
		},
		Features: buildFeatures(in.Bars),
		Position: pos,
	}
	if len(in.IndexBars) > 0 {
		s.Index = Index{
			Available:        true,
			ReturnsPct:       returns(in.IndexBars),
			SessionReturnPct: sessionReturn(in.IndexBars),
		}
	}
	return s, nil
}

// Encode returns s as the JSON value for Jev's `state` field.
func Encode(s Snapshot) (json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("snapshot: encode: %w", err)
	}
	return b, nil
}

func validateAlias(alias string, bars []domain.Bar) error {
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("snapshot: alias %q must match %s", alias, aliasPattern)
	}
	for _, b := range bars {
		if sym := b.Instrument.Symbol; sym != "" && strings.EqualFold(alias, sym) {
			return fmt.Errorf("snapshot: alias must not be the instrument symbol")
		}
	}
	return nil
}

func validateBars(name string, bars []domain.Bar, now, open time.Time) error {
	for i, b := range bars {
		if b.End.After(now) {
			return fmt.Errorf("snapshot: %s[%d] closes after now (look-ahead)", name, i)
		}
		if b.Start.Before(open) {
			return fmt.Errorf("snapshot: %s[%d] starts before session open", name, i)
		}
		if b.Instrument != bars[0].Instrument {
			return fmt.Errorf("snapshot: %s[%d] is a different instrument", name, i)
		}
		if i > 0 && !b.Start.After(bars[i-1].Start) {
			return fmt.Errorf("snapshot: %s[%d] is out of order", name, i)
		}
		if !(b.Open > 0 && b.High > 0 && b.Low > 0 && b.Close > 0) || b.Low > b.High || b.Volume < 0 {
			return fmt.Errorf("snapshot: %s[%d] has invalid prices or volume", name, i)
		}
	}
	return nil
}

func buildFeatures(bars []domain.Bar) Features {
	last := bars[len(bars)-1]
	f := Features{
		ReturnsPct:       returns(bars),
		SessionReturnPct: sessionReturn(bars),
		BarRangePct:      round(pct(last.High-last.Low, last.Close)),
	}

	lo, hi := bars[0].Low, bars[0].High
	var vol, pv float64
	for _, b := range bars {
		lo, hi = math.Min(lo, b.Low), math.Max(hi, b.High)
		v := float64(b.Volume)
		vol += v
		pv += v * (b.High + b.Low + b.Close) / 3
	}
	if hi > lo {
		f.SessionRangePos = ptr(round((last.Close - lo) / (hi - lo)))
	}
	if vol > 0 {
		f.VolumeRel = ptr(round(float64(last.Volume) / (vol / float64(len(bars)))))
		f.VWAPDistPct = ptr(round(pct(last.Close, pv/vol) - 100))
	}
	return f
}

func returns(bars []domain.Bar) Returns {
	ret := func(k int) *float64 {
		if len(bars) <= k {
			return nil
		}
		return ptr(round(pct(bars[len(bars)-1].Close, bars[len(bars)-1-k].Close) - 100))
	}
	return Returns{
		Bars1:  ret(Lookbacks[0]),
		Bars5:  ret(Lookbacks[1]),
		Bars15: ret(Lookbacks[2]),
		Bars30: ret(Lookbacks[3]),
	}
}

func sessionReturn(bars []domain.Bar) float64 {
	return round(pct(bars[len(bars)-1].Close, bars[0].Open) - 100)
}

func buildPosition(cfg Config, p domain.Position, bars []domain.Bar) (Position, error) {
	if p.Quantity == 0 {
		return Position{Side: SideFlat, SizeBucket: SizeNone}, nil
	}
	if p.Instrument != bars[0].Instrument {
		return Position{}, errors.New("snapshot: position is for a different instrument")
	}
	if !(p.AvgPrice > 0) {
		return Position{}, errors.New("snapshot: open position has no average price")
	}
	side, sign := SideLong, 1.0
	if p.Quantity < 0 {
		side, sign = SideShort, -1.0
	}
	notional := math.Abs(float64(p.Quantity)) * p.AvgPrice
	bucket := SizeL
	switch {
	case notional <= cfg.SizeThresholds[0]:
		bucket = SizeS
	case notional <= cfg.SizeThresholds[1]:
		bucket = SizeM
	}
	last := bars[len(bars)-1].Close
	return Position{
		Side:             side,
		SizeBucket:       bucket,
		UnrealizedPnLPct: ptr(round(sign * (pct(last, p.AvgPrice) - 100))),
	}, nil
}

// pct returns a/b as a percentage.
func pct(a, b float64) float64 { return a / b * 100 }

// round keeps four decimals: enough resolution for features, and it stops
// full-precision ratios from carrying more than the model needs.
func round(x float64) float64 {
	r := math.Round(x*1e4) / 1e4
	if r == 0 {
		return 0 // no "-0" in the JSON
	}
	return r
}

func ptr(x float64) *float64 { return &x }
