package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// EnvKeyVault names the Azure Key Vault local commands read secrets from. It
// holds a vault name, never a secret. When it is unset, secrets come from the
// environment alone.
const EnvKeyVault = "JEVTRADER_KEYVAULT"

// azTimeout bounds one az call, which starts a Python process and may refresh
// a token.
const azTimeout = 30 * time.Second

// vaultNames maps each secret's environment variable to its Key Vault secret.
var vaultNames = map[string]string{
	EnvKiteAPIKey:    "kite-api-key",
	EnvKiteAPISecret: "kite-api-secret",
	EnvJevAPIKey:     "jev-api-key",
}

// vaultNamePattern is Azure's rule for a Key Vault name: 3 to 24 letters,
// digits and hyphens. Checking it keeps a stray value from reaching az as a
// flag.
var vaultNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{1,22}[A-Za-z0-9]$`)

// AzRunner runs the az CLI with args and returns its standard output.
type AzRunner func(ctx context.Context, args ...string) ([]byte, error)

// ExecAz is the AzRunner that runs the real az CLI, using whatever `az login`
// session the operator already has.
func ExecAz(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, azTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "az", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the az CLI is not installed or not on PATH")
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("az did not answer within %s", azTimeout)
		}
		// stderr is az's own error text; stdout, which could hold a secret,
		// is never part of an error.
		return nil, fmt.Errorf("az failed: %v: %s", err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// SecretSource loads secrets for a local command: from Azure Key Vault when
// EnvKeyVault names one, otherwise from the environment.
type SecretSource struct {
	// Lookup has the signature of os.LookupEnv.
	Lookup func(string) (string, bool)
	// Az runs the az CLI; nil means ExecAz.
	Az AzRunner
	// Warn receives a line when a vault read fails but the environment
	// supplies the secret instead. It never receives a secret. Nil discards.
	Warn io.Writer
}

// Load reads secrets like LoadSecrets. With EnvKeyVault set, each secret named
// in want (by its environment variable, such as EnvKiteAPIKey) is first read
// from the vault with `az keyvault secret show`, so nothing is stored on disk
// or in an env file; if that read fails or the vault has no value, the
// environment variable is the fallback. A secret neither supplies is reported
// in the error, which also carries the Key Vault failure. Secrets not in want
// come from the environment alone. The returned Secrets are usable even when
// err is not nil.
func (s SecretSource) Load(ctx context.Context, want ...string) (Secrets, error) {
	secrets := LoadSecrets(s.Lookup)
	vault, _ := s.Lookup(EnvKeyVault)
	vault = strings.TrimSpace(vault)
	if vault == "" {
		return secrets, nil
	}
	if !vaultNamePattern.MatchString(vault) {
		return secrets, fmt.Errorf("%s=%q is not a Key Vault name (3-24 letters, digits and hyphens)", EnvKeyVault, vault)
	}
	az := s.Az
	if az == nil {
		az = ExecAz
	}
	var problems []string
	for _, envName := range want {
		name, ok := vaultNames[envName]
		if !ok {
			return secrets, fmt.Errorf("config: no Key Vault secret for %s", envName)
		}
		value, err := readVaultSecret(ctx, az, vault, name)
		if err == nil {
			setSecret(&secrets, envName, value)
			continue
		}
		if have := secretFor(secrets, envName); !have.IsZero() {
			if s.Warn != nil {
				fmt.Fprintf(s.Warn, "warning: key vault %s: %v; using %s from the environment\n", vault, err, envName)
			}
			continue
		}
		problems = append(problems, fmt.Sprintf("key vault %s: %v; %s is not set either", vault, err, envName))
	}
	if len(problems) > 0 {
		return secrets, errors.New(strings.Join(problems, "; "))
	}
	return secrets, nil
}

// readVaultSecret returns one secret's value. Errors never contain it.
func readVaultSecret(ctx context.Context, az AzRunner, vault, name string) (string, error) {
	out, err := az(ctx, "keyvault", "secret", "show", "--vault-name", vault, "--name", name, "--query", "value", "-o", "tsv")
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	return value, nil
}

func setSecret(s *Secrets, envName, value string) {
	v := NewSecret(value)
	switch envName {
	case EnvKiteAPIKey:
		s.KiteAPIKey = v
	case EnvKiteAPISecret:
		s.KiteAPISecret = v
	case EnvJevAPIKey:
		s.JevAPIKey = v
	}
}

func secretFor(s Secrets, envName string) Secret {
	switch envName {
	case EnvKiteAPIKey:
		return s.KiteAPIKey
	case EnvKiteAPISecret:
		return s.KiteAPISecret
	}
	return s.JevAPIKey
}
