# Running and replaying (`run`, `replay`)

Both subcommands build the same engine (`internal/engine`) from different
edges, as described in [reports/0002-architecture.md](../reports/0002-architecture.md).

| | `run` | `replay` |
| --- | --- | --- |
| Market data | Kite ticker | stored ticks, bars or backfilled bars |
| Clock | wall | simulated |
| Decider | Jev | `hold`, `momentum`, `jev`, `recorded:<run-id>` |
| Broker | paper | paper |

## `jevtrader run`

```
jevtrader login
jevtrader run --watchlist NSE:INFY,NSE:TCS
```

Paper trading on live data. The Jev decider uses the pinned model `jev-1.13.0` unless `--model` names another explicit id (`jev-latest` is rejected). It verifies the saved Kite session, resolves the
watchlist (and the `NSE:NIFTY 50` index, used only as snapshot context),
records ticks and closed bars under `--data-dir`, and writes every decision
cycle to the SQLite decision log (`--log`, default `state/decisions.db`).
Creating a `KILL` file in the state directory, or sending `SIGUSR1`, trips the
kill switch.

`--mode=live` cannot place orders: it asks the Kite broker's gate, which
refuses while live trading is not approved in code, and `run` has no live
wiring behind it anyway. Enabling live trading is a separate, reviewed change.

## `jevtrader replay`

```
jevtrader replay --data-dir data --from 2026-10-05 --to 2026-10-09 \
  --deciders hold,momentum,jev
```

Runs the stored data through the engine once per decider. Each run gets its own
run id (`<prefix>-<decider>`) in the decision log, a fresh paper broker and a
simulated clock, and the engine waits for each decision before reading more
data, so the result does not depend on machine speed. Orders fill after the
decision's bar close plus latency (`--latency`, or each decision's measured or
recorded latency), never at the close that triggered them.

The index (`--index`, default `NSE:NIFTY 50`) is found by symbol in the stored
data, kept as snapshot context even under `--tokens`, and never traded.

`recorded:<run-id>` answers from an earlier run's stored answers: a regression
replay of everything except the model. The report says if any snapshot had no
recorded answer.

The report compares net and gross PnL after costs, hit rate (closed round
trips with a positive PnL), maximum drawdown of the equity curve, turnover, and
calibration (the probability given to each BUY or SELL against how often the
price then moved that way `--horizon` bars later). Replays on
`--source backfill` are labelled: Kite candles can differ from tick-built bars.

Backtests are optimistic upper bounds while the model's training cutoff is
unknown; forward paper trading is the deciding evidence.

## Decision log

`internal/decisionlog` is append-only (triggers reject `UPDATE` and `DELETE`).
Tables: `runs`, `cycles` (snapshot, answer, risk verdict, intent; written
before the order is submitted), `order_events` (one per broker update) and
`equity`. Open it with any SQLite client; WAL mode allows reading while a run
is writing.
