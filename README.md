# jev-trader
Realtime trading system using Zerodha Kite data and the Jev decision model

See [ARCHITECTURE.md](ARCHITECTURE.md) for the system overview and the diagrams of each workflow.

## Watchlist

`jevtrader screen` picks the day's instruments before the session: NIFTY Midcap 150 and Smallcap 250 stocks (not NIFTY 50 or Next 50) that are liquid enough for intraday, ranked on news scored by Jev (a material positive news catalyst is required by default) and the pre-open gap. `jevtrader run --watchlist-file` trades the list (idling on a quiet day with no catalysts) and falls back to the static `--watchlist` when the file is missing, stale or unreadable. See [docs/run-and-replay.md](docs/run-and-replay.md#jevtrader-screen) and [reports/0004-watchlist.md](reports/0004-watchlist.md).

`jevtrader study` backtests the picker over several past weeks against buy-and-hold of the picks and a random list of the same size, with hit rates, calibration, which signals went with the morning's return, and Jev's token cost. See [docs/picker-study.md](docs/picker-study.md) and [reports/0005-picker-study.md](reports/0005-picker-study.md).

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on every pull request into `dev` and `main` and on pushes to `dev`. The `test` job runs `go build ./...`, `go vet ./...` and `go test -race ./...`; the `docker` job builds the image (not pushed) and the `bicep` job compiles `infra/main.bicep`. Tests use fixtures only and need no secrets. Superseded runs on the same PR or branch are cancelled. To run the same checks locally:

```sh
go build ./... && go vet ./... && go test -race ./...
```

## Deployment

Merging `dev` into `main` builds the image and tags the Azure VM to run it; the VM is started only on NSE trading mornings and deallocated at 16:00 IST. See [deploy/README.md](deploy/README.md) for the one-time Azure and GitHub setup and the daily Kite login runbook, and [reports/0003-deployment.md](reports/0003-deployment.md) for the design.
