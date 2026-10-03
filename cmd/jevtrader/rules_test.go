package main

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/engine"
)

func parseRules(t *testing.T, args ...string) (*tradingRules, error) {
	t.Helper()
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r := addTradingRuleFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return r, r.validate()
}

func TestTradingRulesDefaultToTradingForHours(t *testing.T) {
	r := defaultRules(t)
	l := tradingLimits(config.Capital{Amount: 25_000}, r)
	if l.MinHold != time.Hour || l.RotationMinHold != time.Hour || l.ReentryCooldown != time.Hour || l.EntryMinProbability != 0.70 || l.EntryConfirmations != 2 {
		t.Errorf("limits %+v", l)
	}
	if r.decideEvery != engine.DefaultDecideEvery || engine.DefaultDecideEvery != 5*time.Minute {
		t.Errorf("decision cadence %s, want 5m", r.decideEvery)
	}
	for _, want := range []string{"decide every 5m0s", "p >= 0.70 on 2 consecutive decisions", "min hold 1h0m0s", "re-entry cooldown 1h0m0s"} {
		if !strings.Contains(r.String(), want) {
			t.Errorf("rules %q lack %q", r, want)
		}
	}
}

func TestTradingRuleFlagsOverrideTheDefaults(t *testing.T) {
	r, err := parseRules(t, "--min-hold", "90m", "--reentry-cooldown", "2h", "--entry-min-probability", "0.8", "--entry-confirmations", "3", "--decide-every", "15m")
	if err != nil {
		t.Fatal(err)
	}
	l := tradingLimits(config.Capital{Amount: 25_000}, r)
	if l.MinHold != 90*time.Minute || l.RotationMinHold != 90*time.Minute || l.ReentryCooldown != 2*time.Hour || l.EntryMinProbability != 0.8 || l.EntryConfirmations != 3 || r.decideEvery != 15*time.Minute {
		t.Errorf("limits %+v, cadence %s", l, r.decideEvery)
	}
	if err := l.Validate(); err != nil {
		t.Error(err)
	}
	off, err := parseRules(t, "--min-hold", "0", "--reentry-cooldown", "0", "--entry-min-probability", "0", "--entry-confirmations", "0", "--decide-every", "1m")
	if err != nil {
		t.Fatal(err)
	}
	if l := tradingLimits(config.Capital{Amount: 25_000}, off); l.MinHold != 0 || l.ReentryCooldown != 0 || l.EntryMinProbability != 0 || l.EntryConfirmations != 0 || l.Validate() != nil {
		t.Errorf("everything off: %+v", l)
	}
}

func TestTradingRuleFlagsAreValidated(t *testing.T) {
	for name, args := range map[string][]string{
		"negative hold":     {"--min-hold", "-1m"},
		"negative cooldown": {"--reentry-cooldown", "-1m"},
		"probability high":  {"--entry-min-probability", "1.5"},
		"probability low":   {"--entry-min-probability", "-0.1"},
		"negative calls":    {"--entry-confirmations", "-1"},
		"cadence zero":      {"--decide-every", "0"},
		"cadence seconds":   {"--decide-every", "90s"},
		"cadence off grid":  {"--decide-every", "7m"},
		"cadence too long":  {"--decide-every", "1h"},
	} {
		if _, err := parseRules(t, args...); err == nil {
			t.Errorf("%s: %v was accepted", name, args)
		}
	}
}

// Every command that trades takes the rules, and a bad one is a usage error.
func TestTradingCommandsTakeTheRuleFlags(t *testing.T) {
	root := writeFixtureTicks(t)
	for _, args := range [][]string{
		{"replay", "--data-dir", root, "--deciders", "hold", "--decide-every", "7m"},
		{"replay", "--data-dir", root, "--deciders", "hold", "--entry-min-probability", "2"},
	} {
		e, _, stderr := testEnv(nil)
		if code := run(context.Background(), args, e); code != 2 {
			t.Errorf("%v: exit %d, want a usage error: %s", args, code, stderr.String())
		}
	}
	for _, cmd := range []string{"run", "replay", "study"} {
		e, _, stderr := testEnv(nil)
		run(context.Background(), []string{cmd, "--help"}, e)
		for _, f := range []string{"-min-hold", "-reentry-cooldown", "-entry-min-probability", "-entry-confirmations", "-decide-every"} {
			if !strings.Contains(stderr.String(), f) {
				t.Errorf("%s --help lacks %s", cmd, f)
			}
		}
	}
}

// The replay report says which rules the runs traded under.
func TestReplayReportNamesTheTradingRules(t *testing.T) {
	root := writeFixtureTicks(t)
	e, stdout, stderr := testEnv(nil)
	args := []string{"replay", "--data-dir", root, "--log", t.TempDir() + "/d.db", "--deciders", "hold", "--run-prefix", "r"}
	if code := run(context.Background(), args, e); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "Trading rules: " + defaultRules(t).String()
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("report lacks %q:\n%s", want, stdout.String())
	}
	if !strings.Contains(stderr.String(), "trading rules: decide every 5m0s") {
		t.Errorf("stderr %q does not print the rules", stderr.String())
	}
}
