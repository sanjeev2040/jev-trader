package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const fixtureSecret = "fixture-secret-value"

func TestSecretRedaction(t *testing.T) {
	s := NewSecret(fixtureSecret)
	type holder struct {
		Key Secret
		Ptr *Secret
	}
	h := holder{Key: s, Ptr: &s}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10s"} {
		outputs = append(outputs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, h))
	}
	outputs = append(outputs, fmt.Sprint(s), fmt.Sprintln(s), s.String(), s.GoString())

	js, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(js))
	txt, err := s.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(txt))

	var logs bytes.Buffer
	for _, handler := range []slog.Handler{slog.NewTextHandler(&logs, nil), slog.NewJSONHandler(&logs, nil)} {
		slog.New(handler).Info("loaded", "secret", s, "holder", h)
	}
	outputs = append(outputs, logs.String())

	for _, out := range outputs {
		if strings.Contains(out, fixtureSecret) {
			t.Errorf("output leaks the secret: %q", out)
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("output %q does not show %s", out, Redacted)
		}
	}
	if s.Reveal() != fixtureSecret {
		t.Errorf("Reveal = %q, want %q", s.Reveal(), fixtureSecret)
	}
}

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadSecrets(t *testing.T) {
	s := LoadSecrets(lookupFrom(map[string]string{
		EnvKiteAPIKey:    "  kite-key\n",
		EnvKiteAPISecret: "kite-secret",
		EnvJevAPIKey:     "jev-key",
	}))
	if s.KiteAPIKey.Reveal() != "kite-key" || s.KiteAPISecret.Reveal() != "kite-secret" || s.JevAPIKey.Reveal() != "jev-key" {
		t.Errorf("LoadSecrets = %q %q %q", s.KiteAPIKey.Reveal(), s.KiteAPISecret.Reveal(), s.JevAPIKey.Reveal())
	}
	if err := s.RequireKite(); err != nil {
		t.Errorf("RequireKite: %v", err)
	}
	if err := s.RequireJev(); err != nil {
		t.Errorf("RequireJev: %v", err)
	}
}

func TestRequireMissing(t *testing.T) {
	s := LoadSecrets(lookupFrom(map[string]string{EnvKiteAPIKey: "kite-key", EnvKiteAPISecret: "   "}))
	err := s.RequireKite()
	if !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("RequireKite err = %v, want ErrMissingSecret", err)
	}
	if !strings.Contains(err.Error(), EnvKiteAPISecret) || strings.Contains(err.Error(), EnvKiteAPIKey+",") {
		t.Errorf("RequireKite err = %q, want only %s named", err, EnvKiteAPISecret)
	}
	if err := s.RequireJev(); !errors.Is(err, ErrMissingSecret) || !strings.Contains(err.Error(), EnvJevAPIKey) {
		t.Errorf("RequireJev err = %v, want ErrMissingSecret naming %s", err, EnvJevAPIKey)
	}
}
