package config

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The trading capital is the total rupee budget the bot may have at work. It
// is configuration, not a credential: it is not a Secret and it is logged. It
// lives in Key Vault only so that it can be changed without a deploy.
const (
	// EnvTradingCapital is the environment variable the budget falls back to.
	// On the VM, prepare.sh sets it from the Key Vault secret.
	EnvTradingCapital = "JEVTRADER_TRADING_CAPITAL"
	// VaultTradingCapital is the Key Vault secret that holds the budget.
	VaultTradingCapital = "trading-capital"
	// DefaultTradingCapital is the budget, in rupees, when nothing sets one.
	DefaultTradingCapital = 25_000
)

// Where a capital budget came from.
const (
	CapitalFromVault   = "key vault"
	CapitalFromEnv     = "environment"
	CapitalFromDefault = "default"
)

// Capital is the resolved trading capital.
type Capital struct {
	Amount float64 // rupees
	Source string  // CapitalFromVault, CapitalFromEnv or CapitalFromDefault
	Detail string  // which vault or variable
}

// ParseTradingCapital parses a budget: a positive, finite number of rupees
// such as 25000 or 25000.50. The value must not be silently replaced by a
// default, so anything else is an error.
func ParseTradingCapital(s string) (float64, error) {
	s = strings.TrimSpace(s)
	v, err := strconv.ParseFloat(s, 64)
	switch {
	case err != nil:
		return 0, fmt.Errorf("trading capital %q is not a number of rupees", s)
	case math.IsNaN(v) || math.IsInf(v, 0) || v <= 0:
		return 0, fmt.Errorf("trading capital %q must be a positive number of rupees", s)
	}
	return v, nil
}

// LoadTradingCapital resolves the trading capital: the Key Vault secret
// trading-capital when EnvKeyVault names a vault, else (or when the vault
// cannot be read) the environment variable EnvTradingCapital, else
// DefaultTradingCapital. A value that is present but is not a positive number
// is an error rather than a fallback, so a typo cannot quietly raise or lower
// the budget.
func (s SecretSource) LoadTradingCapital(ctx context.Context) (Capital, error) {
	var vaultErr error
	vault, _ := s.Lookup(EnvKeyVault)
	if vault = strings.TrimSpace(vault); vault != "" {
		if !vaultNamePattern.MatchString(vault) {
			return Capital{}, fmt.Errorf("%s=%q is not a Key Vault name (3-24 letters, digits and hyphens)", EnvKeyVault, vault)
		}
		az := s.Az
		if az == nil {
			az = ExecAz
		}
		var value string
		if value, vaultErr = readVaultSecret(ctx, az, vault, VaultTradingCapital); vaultErr == nil {
			v, err := ParseTradingCapital(value)
			if err != nil {
				return Capital{}, fmt.Errorf("key vault %s secret %s: %w", vault, VaultTradingCapital, err)
			}
			return Capital{Amount: v, Source: CapitalFromVault, Detail: vault + "/" + VaultTradingCapital}, nil
		}
	}
	if value, ok := s.Lookup(EnvTradingCapital); ok && strings.TrimSpace(value) != "" {
		v, err := ParseTradingCapital(value)
		if err != nil {
			return Capital{}, fmt.Errorf("%s: %w", EnvTradingCapital, err)
		}
		if vaultErr != nil && s.Warn != nil {
			fmt.Fprintf(s.Warn, "warning: key vault %s: %v; using %s from the environment\n", vault, vaultErr, EnvTradingCapital)
		}
		return Capital{Amount: v, Source: CapitalFromEnv, Detail: EnvTradingCapital}, nil
	}
	if vaultErr != nil && s.Warn != nil {
		fmt.Fprintf(s.Warn, "warning: key vault %s: %v; using the default trading capital of %d\n", vault, vaultErr, DefaultTradingCapital)
	}
	return Capital{Amount: DefaultTradingCapital, Source: CapitalFromDefault, Detail: "built-in default"}, nil
}
