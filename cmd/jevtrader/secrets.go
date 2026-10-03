package main

import (
	"context"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

// azRunner runs the az CLI for Key Vault reads. Tests replace it.
var azRunner config.AzRunner = config.ExecAz

// loadSecrets reads the named secrets (by environment variable) through
// config.SecretSource: from the Key Vault that JEVTRADER_KEYVAULT names, with
// the environment as the fallback, or from the environment alone when it is
// unset.
func loadSecrets(ctx context.Context, e env, names ...string) (config.Secrets, error) {
	return config.SecretSource{Lookup: e.lookup, Az: azRunner, Warn: e.stderr}.Load(ctx, names...)
}
