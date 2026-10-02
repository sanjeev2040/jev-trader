# jev-trader
Realtime trading system using Zerodha Kite data and the Jev decision model

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on every pull request into `dev` and `main` and on pushes to `dev`. The `test` job runs `go build ./...`, `go vet ./...` and `go test -race ./...`; the `docker` job builds the image (not pushed) and the `bicep` job compiles `infra/main.bicep`. Tests use fixtures only and need no secrets. Superseded runs on the same PR or branch are cancelled. To run the same checks locally:

```sh
go build ./... && go vet ./... && go test -race ./...
```

## Deployment

Merging `dev` into `main` builds the image and tags the Azure VM to run it; the VM is started only on NSE trading mornings and deallocated at 16:00 IST. See [deploy/README.md](deploy/README.md) for the one-time Azure and GitHub setup and the daily Kite login runbook, and [reports/0003-deployment.md](reports/0003-deployment.md) for the design.
