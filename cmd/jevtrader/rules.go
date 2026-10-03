package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

// tradingRules are the flags that set how patiently the bot trades: how long
// it holds, how sure it must be to enter and how often it decides (issue #53).
// run, replay and study register the same flags with the same defaults, so a
// study of the defaults is a study of what run does.
type tradingRules struct {
	minHold       time.Duration
	cooldown      time.Duration
	entryMinProb  float64
	confirmations int
	decideEvery   time.Duration
}

// addTradingRuleFlags registers the flags on fs.
func addTradingRuleFlags(fs *flag.FlagSet) *tradingRules {
	d := risk.DefaultLimits()
	r := &tradingRules{}
	fs.DurationVar(&r.minHold, "min-hold", d.MinHold, "least time a position is held before a decider's exit or a rotation may sell it, counted from its first entry; the square-off, the daily loss limit and the kill switch are never delayed; 0 turns it off")
	fs.DurationVar(&r.cooldown, "reentry-cooldown", d.ReentryCooldown, "how long after selling out of an instrument before the bot may open a new position in it; 0 turns it off")
	fs.Float64Var(&r.entryMinProb, "entry-min-probability", d.EntryMinProbability, "least probability an entry (a BUY, or a SELL that opens a short) needs; exits keep the lower base threshold; 0 turns it off")
	fs.IntVar(&r.confirmations, "entry-confirmations", d.EntryConfirmations, "consecutive decisions on an instrument that must choose the same BUY or SELL, each above the entry threshold, before it is acted on; 0 or 1 turns it off")
	fs.DurationVar(&r.decideEvery, "decide-every", engine.DefaultDecideEvery, "how often each instrument is put to the decider, aligned to the clock (5m decides at 09:20, 09:25, ...); stops and exits do not wait for it; 1m decides on every bar")
	return r
}

// validate rejects values the gate or the engine would only fail on later.
func (r *tradingRules) validate() error {
	var errs []error
	if r.minHold < 0 || r.cooldown < 0 {
		errs = append(errs, errors.New("--min-hold and --reentry-cooldown must not be negative"))
	}
	if math.IsNaN(r.entryMinProb) || r.entryMinProb < 0 || r.entryMinProb > 1 {
		errs = append(errs, errors.New("--entry-min-probability must be 0 (off) or in (0, 1]"))
	}
	if r.confirmations < 0 {
		errs = append(errs, errors.New("--entry-confirmations must not be negative"))
	}
	if r.decideEvery < time.Minute || r.decideEvery%time.Minute != 0 || (30*time.Minute)%r.decideEvery != 0 {
		errs = append(errs, fmt.Errorf("--decide-every must be a whole number of minutes that divides 30m (1m, 2m, 3m, 5m, 6m, 10m, 15m or 30m), got %s", r.decideEvery))
	}
	return errors.Join(errs...)
}

// apply sets the rules on l. The minimum hold also governs how long a position
// must be held before it can be rotated out, so a rotation is not a way round it.
func (r *tradingRules) apply(l risk.Limits) risk.Limits {
	l.MinHold, l.RotationMinHold = r.minHold, r.minHold
	l.ReentryCooldown = r.cooldown
	l.EntryMinProbability = r.entryMinProb
	l.EntryConfirmations = r.confirmations
	return l
}

// String says what the rules are, for the log and the replay report.
func (r *tradingRules) String() string {
	return fmt.Sprintf("decide every %s; entry needs p >= %.2f on %d consecutive decisions; min hold %s; re-entry cooldown %s",
		r.decideEvery, r.entryMinProb, max(r.confirmations, 1), r.minHold, r.cooldown)
}
