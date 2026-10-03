package config

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
)

func TestParseTradingCapital(t *testing.T) {
	for in, want := range map[string]float64{"25000": 25_000, " 10000\n": 10_000, "12500.50": 12_500.5, "1e5": 100_000} {
		if got, err := ParseTradingCapital(in); err != nil || got != want {
			t.Errorf("ParseTradingCapital(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "0", "-5", "NaN", "Inf", "25,000", "₹25000"} {
		if _, err := ParseTradingCapital(in); err == nil {
			t.Errorf("ParseTradingCapital(%q) succeeded", in)
		}
	}
}

func TestDefaultTradingCapitalIs25000(t *testing.T) {
	if DefaultTradingCapital != 25_000 {
		t.Fatalf("default trading capital = %d, want 25000", DefaultTradingCapital)
	}
}

func TestTradingCapitalFromVault(t *testing.T) {
	az := &fakeAz{values: map[string]string{VaultTradingCapital: "10000"}}
	src := SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test", EnvTradingCapital: "5000"}), Az: az.run}
	c, err := src.LoadTradingCapital(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Amount != 10_000 || c.Source != CapitalFromVault || !strings.Contains(c.Detail, "kv-jev-test/trading-capital") {
		t.Fatalf("capital = %+v, want the vault's 10000", c)
	}
	if len(az.calls) != 1 || !strings.Contains(strings.Join(az.calls[0], " "), "--name trading-capital") {
		t.Errorf("az calls = %v", az.calls)
	}
}

func TestTradingCapitalFallsBackToEnvironmentThenDefault(t *testing.T) {
	down := &fakeAz{values: map[string]string{VaultTradingCapital: "!"}}

	var warn bytes.Buffer
	src := SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test", EnvTradingCapital: " 7500 "}), Az: down.run, Warn: &warn}
	c, err := src.LoadTradingCapital(context.Background())
	if err != nil || c.Amount != 7_500 || c.Source != CapitalFromEnv {
		t.Fatalf("capital = %+v, %v; want 7500 from the environment", c, err)
	}
	if !strings.Contains(warn.String(), "using JEVTRADER_TRADING_CAPITAL from the environment") {
		t.Errorf("warning %q does not say the vault was skipped", warn.String())
	}

	warn.Reset()
	src = SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test"}), Az: down.run, Warn: &warn}
	c, err = src.LoadTradingCapital(context.Background())
	if err != nil || c.Amount != DefaultTradingCapital || c.Source != CapitalFromDefault {
		t.Fatalf("capital = %+v, %v; want the default", c, err)
	}
	if !strings.Contains(warn.String(), "default trading capital") {
		t.Errorf("warning %q does not say the default is used", warn.String())
	}

	// No vault configured: no az call and no warning.
	az := &fakeAz{}
	warn.Reset()
	c, err = SecretSource{Lookup: lookupOf(nil), Az: az.run, Warn: &warn}.LoadTradingCapital(context.Background())
	if err != nil || c.Amount != DefaultTradingCapital || len(az.calls) != 0 || warn.Len() != 0 {
		t.Fatalf("capital = %+v, %v, az calls %v, warning %q", c, err, az.calls, warn.String())
	}
}

func TestInvalidTradingCapitalIsAnErrorNotAFallback(t *testing.T) {
	for name, src := range map[string]SecretSource{
		"vault value": {Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test", EnvTradingCapital: "5000"}), Az: (&fakeAz{values: map[string]string{VaultTradingCapital: "lots"}}).run},
		"vault zero":  {Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test"}), Az: (&fakeAz{values: map[string]string{VaultTradingCapital: "0"}}).run},
		"env value":   {Lookup: lookupOf(map[string]string{EnvTradingCapital: "-1"})},
		"vault name":  {Lookup: lookupOf(map[string]string{EnvKeyVault: "--evil"})},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := src.LoadTradingCapital(context.Background())
			if err == nil {
				t.Fatalf("capital = %+v, want an error", c)
			}
			if math.IsNaN(c.Amount) {
				t.Fatal("NaN amount")
			}
		})
	}
}
