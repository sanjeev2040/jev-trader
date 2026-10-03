package config

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeAz answers `az keyvault secret show` from a map of secret name to value
// and records every call. A name mapped to "!" fails.
type fakeAz struct {
	values map[string]string
	calls  [][]string
}

func (f *fakeAz) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	var name string
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			name = args[i+1]
		}
	}
	v, ok := f.values[name]
	if !ok || v == "!" {
		return nil, errors.New("az failed: ERROR: (SecretNotFound) not found")
	}
	return []byte(v + "\n"), nil
}

func lookupOf(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestLoadWithoutVaultNeverCallsAz(t *testing.T) {
	az := &fakeAz{}
	src := SecretSource{Lookup: lookupOf(map[string]string{EnvKiteAPIKey: " env-key "}), Az: az.run}
	s, err := src.Load(context.Background(), EnvKiteAPIKey, EnvJevAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if s.KiteAPIKey.Reveal() != "env-key" || len(az.calls) != 0 {
		t.Errorf("key %q, az calls %v", s.KiteAPIKey.Reveal(), az.calls)
	}
}

func TestLoadReadsRequestedSecretsFromVault(t *testing.T) {
	az := &fakeAz{values: map[string]string{"kite-api-key": "vk", "kite-api-secret": "vs", "jev-api-key": "vj"}}
	src := SecretSource{Lookup: lookupOf(map[string]string{
		EnvKeyVault: "kv-jev-test", EnvKiteAPIKey: "env-key", EnvJevAPIKey: "env-jev",
	}), Az: az.run}
	s, err := src.Load(context.Background(), EnvKiteAPIKey, EnvKiteAPISecret)
	if err != nil {
		t.Fatal(err)
	}
	// The vault wins over the environment for what was asked for ...
	if s.KiteAPIKey.Reveal() != "vk" || s.KiteAPISecret.Reveal() != "vs" {
		t.Errorf("kite = %q / %q", s.KiteAPIKey.Reveal(), s.KiteAPISecret.Reveal())
	}
	// ... and what was not asked for is never read from it.
	if s.JevAPIKey.Reveal() != "env-jev" || len(az.calls) != 2 {
		t.Errorf("jev %q, %d az calls", s.JevAPIKey.Reveal(), len(az.calls))
	}
	want := []string{"keyvault", "secret", "show", "--vault-name", "kv-jev-test", "--name", "kite-api-key", "--query", "value", "-o", "tsv"}
	if !slices.Equal(az.calls[0], want) {
		t.Errorf("az args = %v, want %v", az.calls[0], want)
	}
}

func TestLoadFallsBackToEnvironment(t *testing.T) {
	az := &fakeAz{values: map[string]string{"kite-api-key": "!", "jev-api-key": ""}}
	var warn bytes.Buffer
	src := SecretSource{Lookup: lookupOf(map[string]string{
		EnvKeyVault: "kv-jev-test", EnvKiteAPIKey: "env-key", EnvJevAPIKey: "env-jev",
	}), Az: az.run, Warn: &warn}
	s, err := src.Load(context.Background(), EnvKiteAPIKey, EnvJevAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if s.KiteAPIKey.Reveal() != "env-key" || s.JevAPIKey.Reveal() != "env-jev" {
		t.Errorf("fallback = %q / %q", s.KiteAPIKey.Reveal(), s.JevAPIKey.Reveal())
	}
	out := warn.String()
	if !strings.Contains(out, "kite-api-key") || !strings.Contains(out, "using KITE_API_KEY from the environment") || !strings.Contains(out, "jev-api-key is empty") {
		t.Errorf("warnings = %q", out)
	}
	if strings.Contains(out, "env-key") || strings.Contains(out, "env-jev") {
		t.Errorf("a warning leaks a secret: %q", out)
	}
}

func TestLoadReportsASecretNeitherSourceHas(t *testing.T) {
	az := &fakeAz{}
	src := SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test"}), Az: az.run}
	_, err := src.Load(context.Background(), EnvKiteAPIKey)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"kv-jev-test", "kite-api-key", "KITE_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestLoadRejectsABadVaultName(t *testing.T) {
	for _, name := range []string{"--help", "a", "kv name", "kv;rm", strings.Repeat("k", 25), "-kv-"} {
		az := &fakeAz{}
		src := SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: name}), Az: az.run}
		if _, err := src.Load(context.Background(), EnvKiteAPIKey); err == nil || len(az.calls) != 0 {
			t.Errorf("vault %q: err %v, az calls %d", name, err, len(az.calls))
		}
	}
}

func TestVaultSecretsStayRedacted(t *testing.T) {
	az := &fakeAz{values: map[string]string{"jev-api-key": "super-secret-value"}}
	src := SecretSource{Lookup: lookupOf(map[string]string{EnvKeyVault: "kv-jev-test"}), Az: az.run}
	s, err := src.Load(context.Background(), EnvJevAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.JevAPIKey.Reveal(); got != "super-secret-value" {
		t.Fatalf("value = %q", got)
	}
	if out := strings.Join([]string{s.JevAPIKey.String()}, ""); strings.Contains(out, "super-secret") {
		t.Errorf("secret printed as %q", out)
	}
}

func TestExecAzMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ExecAz(context.Background(), "version")
	if err == nil || !strings.Contains(err.Error(), "az CLI") {
		t.Errorf("err = %v", err)
	}
}
