// Package config loads jev-trader's configuration. Secrets come from the
// environment only and are wrapped in Secret so they cannot leak into logs,
// JSON or formatted output by accident.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Environment variables that hold secrets. They are never read from YAML.
const (
	EnvKiteAPIKey    = "KITE_API_KEY"
	EnvKiteAPISecret = "KITE_API_SECRET"
	EnvJevAPIKey     = "JEV_API_KEY"
)

// Redacted is what every formatting and encoding path prints for a Secret.
const Redacted = "[redacted]"

// Secret holds a credential. Its String, GoString, Format, MarshalJSON,
// MarshalText and LogValue methods all return Redacted; Reveal is the only way
// to read the value, so every use of the raw credential is explicit.
type Secret struct {
	value string
}

// NewSecret wraps a credential value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the raw credential. Pass it only to the API that needs it.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.value == "" }

func (s Secret) String() string   { return Redacted }
func (s Secret) GoString() string { return Redacted }

// Format redacts every fmt verb, including %d and %x, which would otherwise
// bypass String.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }

// Secrets are the credentials jev-trader reads from the environment.
type Secrets struct {
	KiteAPIKey    Secret
	KiteAPISecret Secret
	JevAPIKey     Secret
}

// SecretsFromEnv reads secrets from the process environment.
func SecretsFromEnv() Secrets { return LoadSecrets(os.LookupEnv) }

// LoadSecrets reads secrets through lookup, which has the signature of
// os.LookupEnv. Surrounding whitespace, common in env files, is trimmed.
// LoadSecrets does not validate; call the Require method for the credentials a
// subcommand needs.
func LoadSecrets(lookup func(string) (string, bool)) Secrets {
	get := func(name string) Secret {
		v, _ := lookup(name)
		return NewSecret(strings.TrimSpace(v))
	}
	return Secrets{
		KiteAPIKey:    get(EnvKiteAPIKey),
		KiteAPISecret: get(EnvKiteAPISecret),
		JevAPIKey:     get(EnvJevAPIKey),
	}
}

// ErrMissingSecret is wrapped by RequireKite and RequireJev when a required
// environment variable is unset or empty.
var ErrMissingSecret = errors.New("missing secret")

// RequireKite checks that the Kite Connect API key and secret are set.
func (s Secrets) RequireKite() error {
	return require(
		namedSecret{EnvKiteAPIKey, s.KiteAPIKey},
		namedSecret{EnvKiteAPISecret, s.KiteAPISecret},
	)
}

// RequireJev checks that the Jev API key is set.
func (s Secrets) RequireJev() error {
	return require(namedSecret{EnvJevAPIKey, s.JevAPIKey})
}

type namedSecret struct {
	env    string
	secret Secret
}

func require(secrets ...namedSecret) error {
	var missing []string
	for _, n := range secrets {
		if n.secret.IsZero() {
			missing = append(missing, n.env)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: set %s in the environment", ErrMissingSecret, strings.Join(missing, ", "))
	}
	return nil
}
