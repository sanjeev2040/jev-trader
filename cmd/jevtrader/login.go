package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/kiteauth"
)

// newKiteClient is replaced in tests to point at a fake Kite server.
var newKiteClient = kiteauth.NewClient

func runLogin(ctx context.Context, args []string, e env) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	redirect := fs.String("redirect-url", kiteauth.DefaultRedirectURL, "redirect URL registered in the Kite developer console (http://127.0.0.1:<port>/<path>)")
	stateDir := fs.String("state-dir", "", "directory for the session file (default $XDG_STATE_HOME/jevtrader or ~/.local/state/jevtrader)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser login")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader login [flags]\n\nLogs in to Kite Connect by hand in a browser and saves the day's access token.\nReads KITE_API_KEY and KITE_API_SECRET from the environment, or from the Azure\nKey Vault named by JEVTRADER_KEYVAULT (kite-api-key, kite-api-secret).\n\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "jevtrader login: unexpected arguments %q\n", fs.Args())
		return 2
	}

	secrets, err := loadSecrets(ctx, e, config.EnvKiteAPIKey, config.EnvKiteAPISecret)
	if err == nil {
		err = secrets.RequireKite()
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "jevtrader login: %v\n", err)
		return 1
	}
	redirectURL, err := kiteauth.ParseRedirectURL(*redirect)
	if err != nil {
		fmt.Fprintf(e.stderr, "jevtrader login: %v\n", err)
		return 2
	}
	if *stateDir == "" {
		if *stateDir, err = kiteauth.DefaultStateDir(); err != nil {
			fmt.Fprintf(e.stderr, "jevtrader login: %v\n", err)
			return 1
		}
	}

	ln, err := net.Listen("tcp", redirectURL.Host)
	if err != nil {
		fmt.Fprintf(e.stderr, "jevtrader login: listen on %s: %v\n", redirectURL.Host, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	flow := &kiteauth.LoginFlow{
		Client:      newKiteClient(secrets),
		Store:       kiteauth.Store{Dir: *stateDir},
		RedirectURL: redirectURL,
	}
	sess, err := flow.Run(ctx, ln, func(loginURL string) {
		fmt.Fprintf(e.stdout, "Open this URL in a browser and log in to Kite (including 2FA):\n\n  %s\n\nWaiting up to %s for the redirect to %s ...\n", loginURL, *timeout, redirectURL)
	})
	if err != nil {
		fmt.Fprintf(e.stderr, "jevtrader login: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "\nLogged in as %s. Session saved to %s; it expires at %s.\n",
		sess.UserID, flow.Store.Path(), sess.ExpiresAt.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(e.stdout, "Note: Kite accepts order placement only from the static IP registered in its developer console; market data works from any IP.")
	return 0
}
