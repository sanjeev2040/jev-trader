package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/risk"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

func TestDefaultCapitalAgreesWithTheRiskGate(t *testing.T) {
	if config.DefaultTradingCapital != risk.DefaultCapitalBudget {
		t.Fatalf("config default %d and risk default %d differ", config.DefaultTradingCapital, risk.DefaultCapitalBudget)
	}
}

func fakeVault(t *testing.T, value string, reads *[]string) {
	t.Helper()
	old := azRunner
	t.Cleanup(func() { azRunner = old })
	azRunner = func(_ context.Context, args ...string) ([]byte, error) {
		name := args[len(args)-5]
		*reads = append(*reads, name)
		if name != config.VaultTradingCapital {
			return nil, errors.New("az failed: not found")
		}
		return []byte(value + "\n"), nil
	}
}

func TestResolveCapitalPrecedence(t *testing.T) {
	var reads []string
	fakeVault(t, "10000", &reads)
	vault := map[string]string{config.EnvKeyVault: "kv-jev-test", config.EnvTradingCapital: "7000"}

	e, _, _ := testEnv(vault)
	c, err := resolveCapital(context.Background(), e, 0)
	if err != nil || c.Amount != 10_000 || c.Source != config.CapitalFromVault {
		t.Fatalf("capital = %+v, %v; want the Key Vault's 10000", c, err)
	}
	// The flag wins and Key Vault is not read.
	reads = nil
	if c, err = resolveCapital(context.Background(), e, 4_000); err != nil || c.Amount != 4_000 || c.Source != capitalFromFlag || len(reads) != 0 {
		t.Fatalf("capital = %+v, %v, vault reads %v; want the flag's 4000 with no vault read", c, err, reads)
	}
	// Without a vault: environment, then default.
	e, _, _ = testEnv(map[string]string{config.EnvTradingCapital: "7000"})
	if c, err = resolveCapital(context.Background(), e, 0); err != nil || c.Amount != 7_000 || c.Source != config.CapitalFromEnv {
		t.Fatalf("capital = %+v, %v; want the environment's 7000", c, err)
	}
	e, _, _ = testEnv(nil)
	if c, err = resolveCapital(context.Background(), e, 0); err != nil || c.Amount != 25_000 || c.Source != config.CapitalFromDefault {
		t.Fatalf("capital = %+v, %v; want the default 25000", c, err)
	}
	for _, bad := range []float64{-1} {
		var u usageError
		if _, err := resolveCapital(context.Background(), e, bad); !errors.As(err, &u) {
			t.Errorf("--capital %v: err = %v, want a usage error", bad, err)
		}
	}
}

func TestInvalidVaultCapitalStopsTheCommand(t *testing.T) {
	var reads []string
	fakeVault(t, "plenty", &reads)
	e, _, stderr := testEnv(map[string]string{config.EnvKeyVault: "kv-jev-test"})
	args := []string{"replay", "--data-dir", writeFixtureTicks(t), "--log", filepath.Join(t.TempDir(), "d.db"), "--deciders", "hold"}
	if code := run(context.Background(), args, e); code == 0 || !strings.Contains(stderr.String(), `trading capital "plenty" is not a number`) {
		t.Fatalf("exit %d, stderr %q: a bad trading-capital value must stop the command, not fall back", code, stderr.String())
	}
}

func TestReplayUsesTheTradingCapital(t *testing.T) {
	var reads []string
	fakeVault(t, "10000", &reads)
	root := writeFixtureTicks(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"key vault": {nil, "capital budget ₹10000 (key vault: kv-jev-test/trading-capital)"},
		"flag":      {[]string{"--capital", "4000"}, "capital budget ₹4000 (flag: --capital)"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, stderr := testEnv(map[string]string{config.EnvKeyVault: "kv-jev-test"})
			args := append([]string{"replay", "--data-dir", root, "--log", filepath.Join(t.TempDir(), "d.db"), "--deciders", "hold"}, tc.args...)
			if code := run(context.Background(), args, e); code != 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q, want %q", code, stderr.String(), tc.want)
			}
		})
	}
}

func TestTradingLimitsScaleWithTheBudget(t *testing.T) {
	l := tradingLimits(config.Capital{Amount: 25_000})
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	s, m := sizeThresholds(l)[0], sizeThresholds(l)[1]
	if s != 1_250 || m != 2_500 || buyHoldNotional(l) != 2_500 {
		t.Fatalf("S %v, M %v, buy-and-hold %v; want 1250, 2500, 2500 for a 25,000 budget", s, m, buyHoldNotional(l))
	}
	if got, _ := tradingLimits(config.Capital{Amount: 10_000}).EntryNotional(domain.SizeL); got != 2_000 {
		t.Fatalf("L entry at 10,000 = %v, want 2000", got)
	}
}

// run trades the picker's list for the day: all fifty names, not a handful.
func TestRunTradesTheFiftyNamesOfTodaysWatchlist(t *testing.T) {
	f := watchlist.File{Date: todayIST(), Phase: watchlist.PhaseFinal, Final: true}
	var want []string
	for i := range 50 {
		key := fmt.Sprintf("NSE:STOCK%02d", i)
		want = append(want, key)
		f.Entries = append(f.Entries, watchlist.Entry{Rank: i + 1, Key: key})
	}
	path := watchlist.Path(t.TempDir(), todayIST())
	if err := watchlist.Write(path, f); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := resolveWatchlist([]string{"NSE:INFY"}, path, time.Now(), logger)
	if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("instruments = %d %v, %v; want the file's 50 in rank order", len(got), got, err)
	}
}
