package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/risk"
)

const capitalFlagHelp = "total rupee budget the bot may have at work (positions it opened plus open buy orders); 0 reads the Key Vault secret trading-capital named by JEVTRADER_KEYVAULT, then " + config.EnvTradingCapital + ", then the built-in 25000"

const capitalFromFlag = "flag"

// resolveCapital returns the trading capital: the --capital flag if given,
// otherwise Key Vault, the environment or the default (config.SecretSource).
func resolveCapital(ctx context.Context, e env, flagValue float64) (config.Capital, error) {
	switch {
	case flagValue == 0:
		return config.SecretSource{Lookup: e.lookup, Az: azRunner, Warn: e.stderr}.LoadTradingCapital(ctx)
	case math.IsNaN(flagValue) || math.IsInf(flagValue, 0) || flagValue < 0:
		return config.Capital{}, usageError{errors.New("--capital must be a positive number of rupees")}
	}
	return config.Capital{Amount: flagValue, Source: capitalFromFlag, Detail: "--capital"}, nil
}

// tradingLimits are the risk limits for a session with the given capital and
// trading rules.
func tradingLimits(c config.Capital, r *tradingRules) risk.Limits {
	l := r.apply(risk.DefaultLimits())
	l.CapitalBudget = c.Amount
	return l
}

// sizeThresholds are the S and M entry sizes the snapshot buckets positions by.
func sizeThresholds(l risk.Limits) [2]float64 {
	s, _ := l.EntryNotional(domain.SizeS)
	m, _ := l.EntryNotional(domain.SizeM)
	return [2]float64{s, m}
}

// logCapital says how much capital the session may use and where that came
// from, and warns when the hard gross cap is what limits it.
func logCapital(logger *slog.Logger, c config.Capital, l risk.Limits) {
	logger.Info("capital budget", "rupees", c.Amount, "source", c.Source, "from", c.Detail, "usable_after_costs", fmt.Sprintf("%.2f", l.Budget()))
	if l.Budget() < c.Amount/(1+l.CostReserveRate) {
		logger.Warn("the hard gross cap is below the capital budget and limits it", "budget", c.Amount, "gross_cap", l.MaxGrossNotional)
	}
}

// buyHoldNotional is what the replay comparison's buy-and-hold benchmark
// spends per instrument and day: the size-M entry, as before.
func buyHoldNotional(l risk.Limits) float64 {
	m, _ := l.EntryNotional(domain.SizeM)
	return m
}
