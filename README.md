# jev-trader
Realtime trading system using Zerodha Kite data and the Jev decision model

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on every pull request into `dev` and `main` and on pushes to `dev`. The `test` job runs `go build ./...`, `go vet ./...` and `go test -race ./...`. Tests use fixtures only and need no secrets. Superseded runs on the same PR or branch are cancelled. To run the same checks locally:

```sh
go build ./... && go vet ./... && go test -race ./...
```
