package risk

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Kite's per-user order limits. The gate's own limits must stay below them.
const (
	kiteOrdersPerSecond = 10
	kiteOrdersPerMinute = 400
	kiteOrdersPerDay    = 5000
)

// IST is India Standard Time. It has no daylight saving, so a fixed zone is
// exact and needs no tzdata.
var IST = time.FixedZone("IST", 5*60*60+30*60)

// TimeOfDay is a wall-clock time within a trading day.
type TimeOfDay struct {
	Hour, Minute int
}

func (t TimeOfDay) minutes() int { return t.Hour*60 + t.Minute }

// On returns t on the calendar day of day, in day's location.
func (t TimeOfDay) On(day time.Time) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d, t.Hour, t.Minute, 0, 0, day.Location())
}

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// Limits configures the risk gate. All money amounts are in INR.
type Limits struct {
	// MinProbability is the minimum probability of the chosen action, per
	// action, for the gate to act on it.
	MinProbability map[domain.Action]float64

	// SizeNotional is the target notional of a new entry, per size bucket.
	SizeNotional map[string]float64

	MaxPositionNotional float64 // per-instrument cap on |quantity| × price
	MaxGrossNotional    float64 // cap on the sum over all instruments, including orders in flight
	MaxOpenPositions    int     // cap on instruments with an open position or entry in flight

	// MaxDailyLoss halts new entries and flattens the book once the day's
	// realized plus unrealized PnL reaches -MaxDailyLoss.
	MaxDailyLoss float64

	MaxOrdersPerSecond int
	MaxOrdersPerMinute int
	MaxOrdersPerDay    int // budget for decision-driven orders; risk-driven exits bypass it

	// MaxConsecutiveRejections trips the kill switch after this many broker
	// rejections in a row.
	MaxConsecutiveRejections int

	EntryStart TimeOfDay // no new entries before this time
	EntryEnd   TimeOfDay // no new entries at or after this time
	SquareOff  TimeOfDay // flatten every open position at or after this time

	AllowShort bool // allow SELL to open a short position

	Location *time.Location // trading-day time zone; nil means IST
}

// DefaultLimits returns conservative limits for a small intraday book.
func DefaultLimits() Limits {
	return Limits{
		MinProbability: map[domain.Action]float64{
			domain.ActionBuy:  0.60,
			domain.ActionSell: 0.60,
		},
		SizeNotional: map[string]float64{
			domain.SizeS: 10_000,
			domain.SizeM: 25_000,
			domain.SizeL: 50_000,
		},
		MaxPositionNotional:      50_000,
		MaxGrossNotional:         200_000,
		MaxOpenPositions:         5,
		MaxDailyLoss:             5_000,
		MaxOrdersPerSecond:       3,
		MaxOrdersPerMinute:       60,
		MaxOrdersPerDay:          500,
		MaxConsecutiveRejections: 3,
		EntryStart:               TimeOfDay{9, 20},
		EntryEnd:                 TimeOfDay{15, 0},
		SquareOff:                TimeOfDay{15, 10},
		AllowShort:               false,
		Location:                 IST,
	}
}

func (l Limits) location() *time.Location {
	if l.Location == nil {
		return IST
	}
	return l.Location
}

// Validate reports every limit that is missing or out of range.
func (l Limits) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	positive := func(name string, v float64) {
		if !(v > 0) || math.IsInf(v, 0) {
			bad("%s must be a positive number, got %v", name, v)
		}
	}

	for _, a := range []domain.Action{domain.ActionBuy, domain.ActionSell} {
		p, ok := l.MinProbability[a]
		if !ok || !(p > 0 && p <= 1) {
			bad("min probability for %s must be in (0, 1], got %v", a, p)
		}
	}
	for _, s := range []string{domain.SizeS, domain.SizeM, domain.SizeL} {
		positive(fmt.Sprintf("size notional for %s", s), l.SizeNotional[s])
	}
	positive("max position notional", l.MaxPositionNotional)
	positive("max gross notional", l.MaxGrossNotional)
	positive("max daily loss", l.MaxDailyLoss)
	if l.MaxGrossNotional < l.MaxPositionNotional {
		bad("max gross notional %v is below max position notional %v", l.MaxGrossNotional, l.MaxPositionNotional)
	}
	if l.MaxOpenPositions < 1 {
		bad("max open positions must be at least 1, got %d", l.MaxOpenPositions)
	}
	if l.MaxOrdersPerSecond < 1 || l.MaxOrdersPerSecond >= kiteOrdersPerSecond {
		bad("max orders per second must be in [1, %d), got %d", kiteOrdersPerSecond, l.MaxOrdersPerSecond)
	}
	if l.MaxOrdersPerMinute < l.MaxOrdersPerSecond || l.MaxOrdersPerMinute >= kiteOrdersPerMinute {
		bad("max orders per minute must be in [max per second, %d), got %d", kiteOrdersPerMinute, l.MaxOrdersPerMinute)
	}
	if l.MaxOrdersPerDay < l.MaxOrdersPerMinute || l.MaxOrdersPerDay >= kiteOrdersPerDay {
		bad("max orders per day must be in [max per minute, %d), got %d", kiteOrdersPerDay, l.MaxOrdersPerDay)
	}
	if l.MaxConsecutiveRejections < 1 {
		bad("max consecutive rejections must be at least 1, got %d", l.MaxConsecutiveRejections)
	}

	for _, t := range []struct {
		name string
		v    TimeOfDay
	}{{"entry start", l.EntryStart}, {"entry end", l.EntryEnd}, {"square-off", l.SquareOff}} {
		if t.v.Hour < 0 || t.v.Hour > 23 || t.v.Minute < 0 || t.v.Minute > 59 {
			bad("%s %s is not a valid time of day", t.name, t.v)
		}
	}
	if l.EntryStart.minutes() >= l.EntryEnd.minutes() {
		bad("entry start %s must be before entry end %s", l.EntryStart, l.EntryEnd)
	}
	if l.EntryEnd.minutes() > l.SquareOff.minutes() {
		bad("entry end %s must not be after square-off %s", l.EntryEnd, l.SquareOff)
	}
	return errors.Join(errs...)
}
