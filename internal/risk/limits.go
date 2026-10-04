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

// DefaultCapitalBudget is the capital budget, in rupees, used when none is
// configured. The deployed value is the Key Vault secret trading-capital.
const DefaultCapitalBudget = 25_000

// The trade-for-hours defaults (issue #53). The picker study found Jev's calls
// right 46-48% of the time over 5 to 30 minutes but 55% by the close, so the
// bot holds for an hour at least, waits an hour before going back into a stock
// it sold, and only enters on a call that is both strong and repeated.
const (
	DefaultMinHold             = 60 * time.Minute
	DefaultReentryCooldown     = 60 * time.Minute
	DefaultEntryMinProbability = 0.70
	DefaultEntryConfirmations  = 2
)

// DefaultStopLossPct is the per-position stop (issue #59): a position is sold
// once it is 2% below its average price, whatever the minimum hold says. A
// round trip costs about 0.15%, so 2% is a loss the bot will take rather than
// a wiggle it will not sit through.
const DefaultStopLossPct = 0.02

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
	// EntryMinProbability is a higher bar for opening or adding to a position:
	// an entry needs at least this probability as well as MinProbability for
	// its action. Exits keep MinProbability alone, so being more choosy about
	// entering never makes a position harder to leave. Zero turns it off.
	EntryMinProbability float64
	// EntryConfirmations is how many consecutive decisions on an instrument
	// must choose the same BUY or SELL, each at or above the entry threshold,
	// before the gate acts on the last of them (ReasonUnconfirmed). A HOLD, the
	// other action or a call below the threshold starts the count again, as
	// does an approved entry and a new trading day. One or zero turns it off.
	EntryConfirmations int

	// CapitalBudget is the total capital, in rupees, the bot may have at work
	// at once: the cost of the positions it opened plus its open buy orders,
	// with CostReserveRate on top. Every entry is sized from what is left of
	// it, and no order may take the total over it.
	CapitalBudget float64
	// CostReserveRate is the estimated round-trip charges as a fraction of an
	// entry's notional. The budget is held back by it: an entry must fit in
	// CapitalBudget after its estimated costs.
	CostReserveRate float64

	// SizeFraction is the target notional of a new entry per size bucket, as
	// a fraction of CapitalBudget. An entry is at least one share and is
	// clipped to the capital that is free.
	SizeFraction map[string]float64

	// MaxPositionFraction caps the capital in one instrument as a fraction of
	// CapitalBudget; MaxPositionNotional caps it in rupees whatever the
	// budget. The lower of the two applies.
	MaxPositionFraction float64
	MaxPositionNotional float64
	MaxGrossNotional    float64 // hard cap on the capital at work, whatever the budget, including orders in flight

	// RotationMargin is how much higher a BUY's probability must be than the
	// probability a position was opened at for the gate to sell that position
	// to make room for the BUY when the budget is full. RotationMinHold is how
	// long a position must have been held before it can be rotated out.
	RotationMargin  float64
	RotationMinHold time.Duration

	// MinHold is how long a position must have been held before a decider's
	// exit is allowed; until then the gate refuses it (ReasonMinHold). It
	// never delays a risk-driven exit: the stop, the time exit, the square-off,
	// the daily loss limit and the kill switch sell at once. A position the book has no
	// entry time for, such as one restored from the journal, is not held back.
	// Zero turns the check off.
	MinHold time.Duration
	// ReentryCooldown is how long after the bot sells out of an instrument
	// (an exit, a risk exit or a rotation) before it may open a new position
	// in it again (ReasonCooldown). Zero turns the check off.
	ReentryCooldown time.Duration

	// StopLossPct is the per-position stop, as a fraction of the position's
	// average price: a long is sold once its last price is that far below it
	// (a short is bought back once it is that far above). StopRangeMult widens
	// it for a volatile stock: the stop distance is the larger of StopLossPct
	// and StopRangeMult times the instrument's price range over RangeWindow,
	// read when the position is opened and then kept, so a falling price cannot
	// widen its own stop. Either may be zero; with both zero there is no stop.
	// The stop is a risk exit like the square-off: Gate.Exits places it on the
	// first mark through the stop level, a decider is not asked, and the
	// minimum hold, the re-entry cooldown and the entry confirmations never
	// delay it (ReasonStopLoss). A gap through the level exits at the gapped
	// price. A position the book has no entry for, such as one restored from
	// the journal, is stopped on StopLossPct alone.
	StopLossPct   float64
	StopRangeMult float64
	// MaxHold is the time exit: a position is sold once it has been held this
	// long, counted from its first entry (ReasonTimeExit). Like the stop it
	// ignores MinHold and is placed by Gate.Exits without a decider. Zero turns
	// it off. A position without an entry time is not timed out.
	MaxHold time.Duration

	// CostAware turns on the cost-aware entry check: an entry is refused
	// unless the expected move beats the round-trip costs plus a margin
	// (ReasonCostEdge). The expected move, in rupees a share, is
	// (2p - 1) x the instrument's price range over RangeWindow, where p is the
	// decision's probability: a coin flip expects nothing and a certain call
	// captures the whole recent range. The costs are CostReserveRate of the
	// price and the margin is CostMargin of the price. The check does not run
	// for an instrument the book has no price history for.
	CostAware   bool
	CostMargin  float64
	RangeWindow time.Duration

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
		CapitalBudget:   DefaultCapitalBudget,
		CostReserveRate: 0.0015,
		SizeFraction: map[string]float64{
			domain.SizeS: 0.05,
			domain.SizeM: 0.10,
			domain.SizeL: 0.20,
		},
		EntryMinProbability:      DefaultEntryMinProbability,
		EntryConfirmations:       DefaultEntryConfirmations,
		MaxPositionFraction:      0.20,
		MaxPositionNotional:      50_000,
		MaxGrossNotional:         200_000,
		RotationMargin:           0.10,
		RotationMinHold:          DefaultMinHold,
		MinHold:                  DefaultMinHold,
		ReentryCooldown:          DefaultReentryCooldown,
		StopLossPct:              DefaultStopLossPct,
		CostAware:                true,
		CostMargin:               0.0005,
		RangeWindow:              30 * time.Minute,
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

// EntryNotional returns the target notional of a new entry in the size
// bucket, in rupees, before it is clipped to the capital that is free.
func (l Limits) EntryNotional(size string) (float64, bool) {
	f, ok := l.SizeFraction[size]
	return f * l.CapitalBudget, ok
}

// PositionCap returns the most capital, in rupees, one instrument may hold.
func (l Limits) PositionCap() float64 {
	c := l.MaxPositionNotional
	if l.MaxPositionFraction > 0 {
		c = min(c, l.MaxPositionFraction*l.CapitalBudget)
	}
	return c
}

// Budget returns the capital, in rupees, the bot may have at work: the
// budget with its estimated costs held back, and never above the hard gross
// cap.
func (l Limits) Budget() float64 {
	return min(l.CapitalBudget/(1+l.CostReserveRate), l.MaxGrossNotional)
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
	positive("capital budget", l.CapitalBudget)
	if !(l.EntryMinProbability == 0 || (l.EntryMinProbability > 0 && l.EntryMinProbability <= 1)) {
		bad("entry min probability must be 0 (off) or in (0, 1], got %v", l.EntryMinProbability)
	}
	if l.EntryConfirmations < 0 {
		bad("entry confirmations must not be negative, got %d", l.EntryConfirmations)
	}
	if !(l.CostReserveRate >= 0 && l.CostReserveRate < 0.1) {
		bad("cost reserve rate must be in [0, 0.1), got %v", l.CostReserveRate)
	}
	for _, s := range []string{domain.SizeS, domain.SizeM, domain.SizeL} {
		if f := l.SizeFraction[s]; !(f > 0 && f <= 1) {
			bad("size fraction for %s must be in (0, 1], got %v", s, f)
		}
	}
	if f := l.MaxPositionFraction; !(f >= 0 && f <= 1) {
		bad("max position fraction must be in [0, 1], got %v", f)
	}
	if !(l.RotationMargin >= 0 && l.RotationMargin < 1) || l.RotationMinHold < 0 {
		bad("rotation margin must be in [0, 1) and the minimum hold not negative, got %v and %s", l.RotationMargin, l.RotationMinHold)
	}
	if l.MinHold < 0 || l.ReentryCooldown < 0 {
		bad("minimum hold and re-entry cooldown must not be negative, got %s and %s", l.MinHold, l.ReentryCooldown)
	}
	if !(l.StopLossPct >= 0 && l.StopLossPct < 1) {
		bad("stop loss must be a fraction in [0, 1) (0 is off), got %v", l.StopLossPct)
	}
	if !(l.StopRangeMult >= 0) || math.IsInf(l.StopRangeMult, 0) {
		bad("stop range multiple must be 0 (off) or positive, got %v", l.StopRangeMult)
	}
	if l.MaxHold < 0 {
		bad("max hold must not be negative, got %s", l.MaxHold)
	}
	if !(l.CostMargin >= 0 && l.CostMargin < 0.1) {
		bad("cost margin must be in [0, 0.1), got %v", l.CostMargin)
	}
	if (l.CostAware || l.StopRangeMult > 0) && !(l.RangeWindow > 0 && l.RangeWindow <= rangeKeep) {
		bad("range window must be in (0, %s] when the cost-aware check or the range stop is on, got %s", rangeKeep, l.RangeWindow)
	}
	positive("max position notional", l.MaxPositionNotional)
	positive("max gross notional", l.MaxGrossNotional)
	positive("max daily loss", l.MaxDailyLoss)
	if l.MaxGrossNotional < l.MaxPositionNotional {
		bad("max gross notional %v is below max position notional %v", l.MaxGrossNotional, l.MaxPositionNotional)
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
