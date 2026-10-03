# Kite Connect login

Kite access tokens expire at 06:00 IST every day, or earlier after a logout. Each trading day starts with a manual `jevtrader login`. jev-trader never logs in on its own: it does not store your Kite password or 2FA seed. See [reports/0002-architecture.md](../reports/0002-architecture.md).

## One-time setup

1. In the [Kite developer console](https://developers.kite.trade/), set the app's redirect URL to `http://127.0.0.1:8765/kite/callback`. To use another port or path, register it and pass it with `-redirect-url`. It must be plain `http` on `127.0.0.1` with an explicit port.
2. For live order placement, register the trading host's static IP in the same console. Since 2026-04-01, Kite rejects order calls from any other IP. Login, market data, `record`, `replay`, `backfill` and paper trading work from any IP.
3. Provide the secrets as environment variables:

   | Variable | Used by |
   | --- | --- |
   | `KITE_API_KEY` | `login`, Kite market data and orders |
   | `KITE_API_SECRET` | `login` (token exchange) |
   | `JEV_API_KEY` | Jev decisions |

   In production, load them with systemd's `EnvironmentFile=` from a `0600` file outside the repository. Locally, keep them in `.env` (ignored by git) and export them with `set -a; . ./.env; set +a`. Secrets and the access token are held in a type that formats, logs and marshals as `[redacted]`. The API key does appear in the printed login URL, because Kite's login page needs it.

## Daily login

Before 09:15 IST:

```sh
jevtrader login
```

It prints a Kite login URL and waits (5 minutes by default; see `-timeout`). Open the URL, log in and complete 2FA. Kite redirects the browser to the local listener, which exchanges the `request_token` for the day's access token and saves it.

On a remote VM, tunnel the listener port first, then open the URL in your local browser:

```sh
ssh -L 8765:127.0.0.1:8765 trader-vm
```

The session is saved to `$XDG_STATE_HOME/jevtrader/kite-session.json`, or `~/.local/state/jevtrader/kite-session.json` when `XDG_STATE_HOME` is unset (`-state-dir` overrides the directory). The file has mode `0600` and is refused if anyone else can read it. Running `jevtrader login` again replaces it.

## When the token is rejected

Code that needs Kite gets the token from `kiteauth.Auth`. It returns an error wrapping `kiteauth.ErrLoginRequired` when there is no session, it has expired, or Kite rejects it with a `TokenException`. Per the architecture, `run` checks the token with a profile call (`Auth.Verify`) at startup and will not trade without one, and a `TokenException` mid-session (`kiteauth.IsTokenException`) stops new entries and raises an alert instead of logging in again. Run `jevtrader login` and restart the service.
