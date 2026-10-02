# 0001 — Implementation language

- **Status:** Proposed
- **Date:** 2026-10-02
- **Issue:** [#1](https://github.com/sanjeev2040/jev-trader/issues/1)
- **Decider:** repository owner (after review of this report)

## Context

jev-trader is a realtime NSE trading system. The pipeline is:

1. **Ingest:** Zerodha Kite Connect. Ticks come over the KiteTicker WebSocket. Historical candles and orders go over REST.
2. **Build bars** from ticks.
3. **Snapshot:** serialize market state into a snapshot.
4. **Decide:** send the snapshot to **Jev**, TypeSafe AI's hosted "System One" model. Jev returns typed answers (for example a `choice` of BUY/SELL/HOLD) with calibrated probabilities. TypeSafe quotes 70–500 ms end to end ([blog](https://typesafe.ai/blog/introducing-system-one-models-and-jev)).
5. **Act:** a deterministic risk gate, then an order manager (paper first, live later).

The language has to balance **ease of use**, **speed** and **cost-effectiveness**. Go and Rust were floated as candidates. Python is included as the baseline because both upstream vendors ship official Python SDKs.

An important constraint shapes every criterion below: **the decision loop is bounded by Jev's network round trip (70–500 ms), not by CPU.** Kite's own limits also cap the ingest side:

- at most 3 WebSocket connections × 3,000 instruments, with binary packets of 8/44/184 bytes for LTP/quote/full mode ([WebSocket docs](https://kite.trade/docs/connect/v3/websocket/));
- 10 orders/s, 400/min and 5,000/day ([rate limits](https://kite.trade/docs/connect/v3/exceptions/)).

Even the absolute ceiling of 9,000 instruments in full mode is a few MB/s of fixed-layout binary parsing. Any of the three languages can handle that. The realistic workload is a watchlist of tens to a few hundred symbols.

## Options

| Option | Summary |
| --- | --- |
| **Go** | Single statically linked service. Uses Zerodha's official `gokiteconnect` and a thin hand-written Jev HTTP client. |
| **Rust** | Single service on tokio. Uses a community Kite crate (or our own) and a hand-written Jev client. |
| **Python** | asyncio/Twisted service. Uses Zerodha's official `kiteconnect` and TypeSafe's official `typesafe-sdk`. |

Hybrids (for example, Rust ingest feeding a Python decision process) were not considered. They add an IPC boundary and two toolchains, with no latency benefit while Jev dominates the loop.

## Criteria matrix

Scores run from 1 (poor) to 5 (strong). Weights reflect the owner's three priorities: ease of use maps to dev ease and testing, speed to runtime, and cost to hosting and maintenance. They also reflect that this system moves real money. The weights are a judgment call; the owner should re-weight if priorities differ.

| Criterion | Weight | Go | Rust | Python |
| --- | ---: | :-: | :-: | :-: |
| Development ease and iteration speed | 20 | 4 | 2 | 5 |
| Kite Connect client maturity (ticker + orders) | 20 | 5 | 2 | 4 |
| Operational safety (types, deploy, failure modes) | 15 | 4 | 5 | 2 |
| Testing and backtest/replay ergonomics | 15 | 4 | 4 | 5 |
| Runtime latency/throughput (ticks, bars) | 10 | 4 | 5 | 3 |
| Effort to call Jev's HTTP API | 5 | 4 | 4 | 5 |
| Memory/hosting cost on a small always-on VM | 5 | 4 | 5 | 3 |
| Concurrency model fit | 5 | 5 | 4 | 3 |
| Storage ecosystem (Parquet, TimescaleDB) | 5 | 4 | 4 | 5 |
| **Weighted total (out of 5.00)** | 100 | **4.25** | **3.50** | **3.95** |

**Sensitivity.** Go leads Python by a narrow margin, and that margin depends on two criteria: operational safety and Kite client maturity. Suppose the 15 points for operational safety moved to development ease. Python would then score 4.40 against Go's 4.25. In other words, Python wins if raw iteration speed matters more than compile-time guarantees in a system that places live orders. Rust's lead on speed is worth at most 0.1–0.2 points, because speed is not the bottleneck.

## Evidence

All repository data was read from GitHub, crates.io, PyPI and the Go module proxy on 2026-10-02.

### Kite Connect clients

**Python: `zerodha/pykiteconnect`** ([repo](https://github.com/zerodha/pykiteconnect))
- Official and MIT-licensed, with about 1.3k stars.
- Latest release is v5.2.2 (2026-09-15); v5.2.1 and v5.2.0 came out in 2026-07 and 2026-04.
- Covers REST, orders, GTT and the ticker, including order updates and auto-reconnect.
- The ticker runs on **Twisted** and pins **`autobahn[twisted]==19.11.2`**, a release from 2020-01-09 ([PyPI](https://pypi.org/pypi/kiteconnect/json)). It does not use asyncio. Mixing it with an asyncio Jev client means bridging two event loops or running the ticker in its threaded mode.

**Go: `zerodha/gokiteconnect`** ([repo](https://github.com/zerodha/gokiteconnect), module `github.com/zerodha/gokiteconnect/v4`)
- **Official**, and maintained by Zerodha alongside the Python client: v4.4.3 shipped on 2026-09-15, the same day as pykiteconnect v5.2.2.
- About 213 stars and 10 open issues.
- Covers orders, GTT, margins, portfolio, market data, mutual funds and alerts, each with `_test.go` files.
- `ticker/ticker.go` supports LTP/quote/full modes, `OnTick`, `OnOrderUpdate`, auto-reconnect with configurable retries and delay, and `ServeWithContext` for clean shutdown.
- It depends on `gorilla/websocket` v1.4.2. That library still works, but its last release was v1.5.3 (2024-06).

**Rust: no maintained official client.**
- Zerodha's [`kiteconnect-rs`](https://github.com/zerodha/kiteconnect-rs) (crate `kiteconnect` 0.2.9) is **archived**. Its last push was 2021-03 and it depends on reqwest 0.9 and the synchronous `ws` crate.
- Community crates are all young and mostly single-maintainer:
  - [`zrsclient`](https://gitlab.com/aphrodite10944/zrsclient): REST + WebSocket with retry and reconnect, v0.3.0 (2026-07), about 42 recent downloads.
  - [`kiteconnect-async-wasm`](https://github.com/SPRAGE/kiteconnect-async-wasm): REST only, last push 2025-08, 2 stars.
  - [`kiteticker-async-manager`](https://github.com/SPRAGE/kiteticker-async-manager): ticker only, one contributor, 2 stars.
  - [`manja`](https://github.com/saxena-dev/manja): v0.2.0 (2026-09), 1 star.
  - [`kiteticker-async`](https://github.com/kaychaks/kiteticker-async): last push 2024-05.
- NautilusTrader has registered a `nautilus-zerodha` crate, but it is a 0.0.0 "bootstrap" placeholder with no Zerodha adapter in the repo yet ([nautilus_trader](https://github.com/nautechsystems/nautilus_trader)).
- **Unverified:** the order-placement coverage and correctness of these crates. A Rust choice would realistically mean owning our own Kite client.

**Shared test fixtures.** Zerodha publishes [`kiteconnect-mocks`](https://github.com/zerodha/kiteconnect-mocks), canned API responses that work from any language.

### Jev (TypeSafe) API

- The API is a single endpoint: `POST https://api.typesafe.ai/v1/systemone` with a bearer token and a JSON body of `{state, model, questions}`. It returns typed `noul`/`choice`/`score` answers ([API reference](https://docs.typesafe.ai/api.md)).
- **Official SDKs:** Python ([`typesafe-sdk-python`](https://github.com/typesafe-ai/typesafe-sdk-python), v0.7.2, 2026-09-26, sync and async clients, optional HTTP/2, retry policy) and TypeScript ([`typesafe-sdk-js`](https://github.com/typesafe-ai/typesafe-sdk-js), v0.6.0). There is **no Go or Rust SDK** ([SDK list](https://docs.typesafe.ai/sdk.md)).
- In Go or Rust, a typed client for the three question types plus retry/timeout is an estimated 150–300 lines over `net/http` or `reqwest` (estimate, not measured). Because the API is one endpoint, it is cheap to hand-write.
- **Pricing:** $0.042 per million input tokens, with free output ([blog](https://typesafe.ai/blog/introducing-system-one-models-and-jev)). Jev cost scales with snapshot size and call rate, not with the language.

### Runtime, concurrency and cost

- **Latency.** Bar building is fixed-layout binary decoding plus integer/float aggregation. Go and Rust do this in microseconds per packet. CPython is roughly an order of magnitude slower per packet (general expectation, not benchmarked here). At realistic watchlist sizes, all three stay far below Jev's 70 ms floor. Go's GC pauses are sub-millisecond and irrelevant against a 70–500 ms decision loop.
- **Concurrency.** The natural shape is ticker → bar builder → snapshot → Jev → risk gate → orders, with timeouts and cancellation.
  - Go: goroutines, channels and `context.Context` fit this shape directly.
  - Rust: tokio fits it too, at the cost of async lifetimes and `Send`/`Sync` friction.
  - Python: asyncio fits it, but the Kite ticker is Twisted-based, and the GIL limits parallel CPU work.
- **Memory and hosting (estimates, unverified).**
  - A Go service like this typically idles at about 20–50 MB RSS, and a Rust one at about 5–20 MB.
  - A Python service with pandas/pyarrow loaded commonly uses 150–300 MB.
  - All three fit on the smallest always-on VM class (1–2 GB RAM), so **the hosting cost difference is negligible** (low single-digit dollars per month at most).
  - The larger cost lever is engineering time, which favours Python and Go over Rust.
- **Deployment.** Go and Rust ship as one static binary. Python needs a managed interpreter and virtualenv, and dependency pins such as the 2020 autobahn pin above.

### Testing, replay and storage

- **Replay/backtest.** The system should be built so the ticker is an interface: live = KiteTicker, replay = recorded ticks from Parquet. Then the same bar → snapshot → decision → risk code runs in both modes.
  - Go: interfaces, `httptest` (to fake Jev) and table tests handle this well.
  - Rust: traits give the same result.
  - Python: the weakest place for the hot path, but the best for *analysing* backtest results (pandas, Polars, notebooks).
- **Storage** (all active):
  - TimescaleDB is PostgreSQL, so the client is [`pgx`](https://github.com/jackc/pgx) v5.11.0 (2026-09) for Go, [`sqlx`](https://github.com/launchbadge/sqlx) v0.9 / [`tokio-postgres`](https://github.com/sfackler/rust-postgres) for Rust, and [`psycopg`](https://github.com/psycopg/psycopg) 3 for Python.
  - Parquet: [`arrow-go`](https://github.com/apache/arrow-go) v18.8.0 (2026-09) and [`parquet-go`](https://github.com/parquet-go/parquet-go) v0.32 for Go, [`arrow-rs`/`parquet`](https://github.com/apache/arrow-rs) 60.0.0 (2026-09) for Rust, and [`pyarrow`](https://github.com/apache/arrow) 25.0 / [Polars](https://github.com/pola-rs/polars) for Python.
  - Python's data tooling is the richest. Go and Rust are fully adequate for writing ticks and bars.

## Recommendation

**Go** for the trading service: ingest, bars, snapshot, Jev client, risk gate and order manager.

1. **The Kite side is the riskiest integration, and Go has an official, actively released client.** It has ticker, order updates and reconnect support, and ships on the same cadence as the Python client. Rust has none maintained, so we would own the most safety-critical adapter ourselves.
2. **Speed is not the bottleneck.** Jev's 70–500 ms round trip dominates, so Rust's raw performance edge buys nothing measurable. Its slower iteration costs real time.
3. **It is easier to operate than Python and safer for an order-placing system.** Static types catch shape errors at compile time, deployment is a single binary, and memory is low. It also avoids the Twisted/asyncio split in Python's Kite ticker.
4. **The Jev gap is small.** The API is one JSON endpoint, so a thin typed Go client is a contained task.

**Python stays welcome off the hot path.** It is the right tool for research notebooks and for analysing backtest output. The Go service writes Parquet that pandas/Polars read directly. Python is also the fallback if the owner weights iteration speed over compile-time safety (see Sensitivity).

## Consequences

- **We must write and maintain a Go Jev client** (typed `noul`/`choice`/`score`, timeouts, retries, error mapping). We should watch for an official Go SDK and switch if one appears.
- **Build-time to-dos:**
  - Wrap `gokiteconnect`'s ticker behind our own interface so replay and tests do not depend on it.
  - Track its `gorilla/websocket` dependency and its open issues.
- **Backtest tooling splits in two:** replay runs in Go, and analysis happens in Python over Parquet. The Parquet schema becomes a contract between the two.
- **Contributors need Go.** Rust is ruled out for now. Revisiting it is worthwhile only if a CPU-bound component appears, such as in-process feature computation over thousands of instruments.
- **Before committing, a short spike would turn the estimates above into measurements:** decode a recorded tick stream, build 1-minute bars, call Jev, and measure p50/p99 latency and RSS.

## Status

**Proposed.** The repository owner makes the final pick after review. On acceptance, change this field to `Accepted (Go)`, or to the chosen alternative, and date it.
