# Running and replaying (`run`, `replay`)

Both subcommands build the same engine (`internal/engine`) from different
edges, as described in [reports/0002-architecture.md](../reports/0002-architecture.md).

`jevtrader shadow-report` reads what `run` logged about every signal, traded or not: see [the shadow log](#the-shadow-log-and-jevtrader-shadow-report).

`jevtrader screen` runs before either: it writes the day's [watchlist file](#jevtrader-screen) that `run` and `replay` can read. `jevtrader study` runs screen, backfill and replay over many past days and aggregates them: see [picker-study.md](picker-study.md).

| | `run` | `replay` |
| --- | --- | --- |
| Market data | Kite ticker | stored ticks, bars or backfilled bars |
| Clock | wall | simulated |
| Decider | Jev | `hold`, `momentum`, `jev`, `recorded:<run-id>` |
| Broker | paper | paper |

## `jevtrader screen`

```
jevtrader login
jevtrader screen                       # before 09:08 IST: the early pass; from 09:08: the final pass
jevtrader screen --date 2026-10-01     # rebuild a past day's list
```

Builds the day's watchlist and writes `state/watchlist/YYYY-MM-DD.json` (`--out-dir`). The design, every source and its limits are in [reports/0004-watchlist.md](../reports/0004-watchlist.md). In short:

1. **Universe**: NIFTY Midcap 150 and Smallcap 250 constituents (NSE's public index CSVs, cached per day under `--data-dir`), less NIFTY 50 and NIFTY Next 50, less stocks not in the `EQ` series or on NSE's ASM/GSM lists.
2. **Liquidity** (Kite daily candles): previous-day traded value ≥ ₹10 crore (`--min-value-cr`), average volume ≥ 200,000 (`--min-adv`), previous close ₹50–₹5,000 (`--min-price`, `--max-price`).
3. **Signals**: *news* from the last 18 hours (`--news-window`) — NSE corporate announcements and five business-news RSS feeds (`--news-sources`) — mapped to the screened companies and scored by Jev with three typed questions (expected impact: Score; material and price-moving: Noul; direction: Choice); the pre-open *gap* (final pass); *sector* strength and recent *range*, which the picker study found carry no information or a negative one and so weigh nothing by default. Combined with the weights `--w-news 0.6 --w-sector 0 --w-gap 0.25 --w-range 0` (a negative `--w-range` penalises a wide range); a signal that is unavailable this run is left out rather than counted as zero. A stock is listed only if Jev scored at least one of its news items as a catalyst: material, clearly positive and worth about 1% or more (`--require-catalyst`, `--catalyst-material 0.5`, `--catalyst-direction 0.3`, `--catalyst-impact 1`; `--require-catalyst=false` ranks every screened stock).
4. **Output**: up to `--top` (50) stocks, so fewer on a quiet day and none on a day with no catalyst (a valid empty list: "No catalysts today", exit 0; `run` then idles). A stock counts as a catalyst only if its net, decayed news signal is positive too. `--direction both` is rejected while the catalyst is required; at most `--max-per-sector` (8) from one sector, ranked by likelihood to rise, each with its score, signals, plain-text reasons and the news behind it. The summary also prints on stdout.

`--phase` is `auto` by default: *early* before 09:08 IST (news and the previous session), *final* from 09:08 (adds the pre-open gap and sectoral indices), *historic* for a past `--date` (no pre-open exists; RSS feeds keep no history, so only NSE filings supply news). An early pass does not replace a final file unless `--force`. Daily candles and Jev's news scores are cached for the day, so the second pass is short.

It needs a Kite session (`jevtrader login`) and reads `KITE_API_KEY` and `JEV_API_KEY` from the environment or from the Key Vault named by `JEVTRADER_KEYVAULT`. Without `JEV_API_KEY` the news is fetched but not scored (a warning says so), and with the catalyst required no list is written. The command makes about 390 Kite day-candle requests (rate limited to 3 a second) on the first pass of the day and the Jev calls cost cents a month.

## `jevtrader run`

```
jevtrader login
jevtrader run --watchlist NSE:INFY,NSE:TCS
jevtrader run --watchlist-file state/watchlist/2026-10-05.json --watchlist NSE:INFY,NSE:TCS
```

`--watchlist-file` trades the instruments `screen` wrote, but only if the file is for today's IST date; if it is missing, stale or unreadable, `run` logs why and trades `--watchlist` instead. A today's file with no entries is a quiet day, not a failure: `run` does not fall back, idles for the session and logs why. With neither a usable file nor `--watchlist`, `run` refuses to start.

`run` trades the whole picker list for the day (up to 50 mid and small caps) as the session's instruments, within a [capital budget](#capital-budget).

Paper trading on live data. The Jev decider uses the pinned model `jev-1.13.0` unless `--model` names another explicit id (`jev-latest` is rejected). It verifies the saved Kite session, resolves the
watchlist (and the `NSE:NIFTY 50` index, used only as snapshot context),
records ticks and closed bars under `--data-dir`, and writes every decision
cycle to the SQLite decision log (`--log`, default `state/decisions.db`).
It also shadow-logs every candidate signal, traded or not (`--shadow`, on by
default; see [the shadow log](#the-shadow-log-and-jevtrader-shadow-report)).
Creating a `KILL` file in the state directory, or sending `SIGUSR1`, trips the
kill switch.

`--mode=live` cannot place orders: it asks the Kite broker's gate, which
refuses while live trading is not approved in code, and `run` has no live
wiring behind it anyway. Enabling live trading is a separate, reviewed change.

## Capital budget

The bot trades with a total rupee budget, **₹25,000** unless configured. It is a limit on the capital the bot has at work, and the risk gate (`internal/risk`) enforces it on every order:

- **What counts.** The cost (average price × quantity) of the positions the bot opened, plus its open buy orders. Estimated costs are held back: the usable budget is `budget / (1 + 0.15%)`, so a 25,000 budget has about 24,963 at work at most. The hard gross cap in `risk.DefaultLimits` (₹200,000) is a ceiling above any configured budget.
- **Sizing.** An entry is sized from the budget: size bucket S, M or L is 5%, 10% or 20% of it (₹1,250, ₹2,500, ₹5,000 at 25,000), never less than one share, clipped to the capital that is free, and to 20% of the budget in one stock. If not even one share fits, the entry is skipped with `budget_full` and says why in the decision log (`committed 24000.00 of budget 25000.00 ...: one share at 1000.00 does not fit`).
- **Rotation.** When the budget is full and a BUY arrives that is stronger than a position the bot holds, the gate sells that position to free the capital. Only a position the bot opened today and has held for at least the minimum hold (an hour) can go, only if the BUY's probability beats the probability it was opened at by 0.10, and only the weakest qualifying one, and only if selling it frees enough for one share. The sale is logged as its own cycle (`source risk`, exit reason `rotation`); when it fills, the engine asks the gate again for the BUY (cycle `<id>-rot-buy`, abandoned after 2 minutes), so the capital is never spent twice. Both orders obey the order-rate limits.
- **Churn.** Four checks keep the bot from trading in and out of a stock for nothing; replays of 2026-10-01 showed RELIANCE round-tripping four times inside a 2-rupee range for -₹249, mostly costs. Each refusal is a gate verdict with its numbers in the decision log and in the replay walkthrough (`gate min_hold (...)`):
  - **Minimum hold** (`min_hold`, 60 minutes; `--min-hold`). A decider's exit is refused until the position has been held that long, counted from its first entry. The stop, the time exit, the square-off, the daily loss limit and the kill switch are not delayed. A position without an entry time (restored from the journal) is not held back.
  - **Re-entry cooldown** (`reentry_cooldown`, 60 minutes; `--reentry-cooldown`). After the bot sells out of an instrument (a decider exit, a risk exit or a rotation) it may not open a new position in it until the cooldown has passed. Other instruments are not affected.
  - **Cost-aware entry** (`cost_edge`). An entry is refused unless the expected move beats the round-trip costs plus a margin. Expected move = (2p − 1) × the instrument's high-to-low range over the last 30 minutes, where p is the decision's probability; it needs at least costs (the 0.15% `CostReserveRate`) plus a 0.05% margin of the price. At 1,400 that is ₹2.80 a share, so a 2-rupee range is refused at any probability, while a 12-rupee range admits p = 0.8 (7.20). The range comes from the ticks (or bar highs and lows) the engine feeds the book; with no price history yet the check is skipped.

  - **Entry bar and confirmation** (`below_probability_threshold`, `entry_unconfirmed`). See [Trading for hours](#trading-for-hours).
- **Stop and time exit.** Every position has a stop, and may have a time exit (issue #59; both in [Trading for hours](#trading-for-hours)). They are risk exits like the square-off, not decisions: `Gate.Exits` checks every position on every tick or bar and sells the one that has gone through its stop (`stop_loss`) or been held for `--max-hold` (`time_exit`), without asking the decider and whatever the minimum hold, the cooldown or the entry confirmations say. They do not count against the 500-a-day order budget but obey the order-rate limits. The sale starts the re-entry cooldown, and its capital is free for the next entry as soon as it fills (no rotation needed). Rotation never picks a position whose stop is already working. A stop that finds an add-on entry still open cancels it first.

  All of it is in `risk.Limits` (`MinHold`, `ReentryCooldown`, `EntryMinProbability`, `EntryConfirmations`, `CostAware`, `CostMargin`, `RangeWindow`, with `CostReserveRate`, and `StopLossPct`, `StopRangeMult`, `MaxHold`); a zero hold, cooldown, stop or time exit, or `CostAware = false`, turns a check off.
- **Many trades.** Buying and selling the same stock several times a day is allowed across all 50 names, subject to the churn checks above and within the order-rate limits (3 a second, 60 a minute, 500 a day) and the daily loss limit (₹5,000), which still halt new entries.
- **Holdings it did not open are out of bounds.** The bot never sells, modifies or otherwise touches a holding or position it did not open, such as your existing Zerodha holdings. Every position in its book comes from its own fills; anything else is registered as foreign (`Book.SetForeign`), and the gate refuses every order in it (`foreign_position`), counts nothing of it against the budget and never flattens it. `internal/risk/budget_test.go` and `internal/engine/rotation_test.go` test this as an invariant.

### Trading for hours

The 2026-09-14 to 2026-10-01 picker study (13 days, 09:15–10:15, ₹25,000) found Jev's BUY and SELL calls right 46–48% of the time at 5, 15 and 30 minutes but 54.9% at the day's close, with flat calibration (its 90%-plus calls were right about 47% of the time). So the defaults now hold for hours and enter less often. Each rule is a flag on `run`, `replay` and `study` with the same default, so a `study` of the defaults is a study of what `run` does, and each is shown in the walkthrough (the report's `Trading rules:` line, and `gate <reason> (...)` on every refused decision).

| Flag | Default | Rule |
| --- | --- | --- |
| `--min-hold` | `1h` | A decider's exit, and a rotation, may not sell a position until it has been held this long. `0` turns it off. |
| `--reentry-cooldown` | `1h` | After selling out of an instrument, no new position in it for this long. `0` turns it off. |
| `--entry-min-probability` | `0.70` | An entry needs at least this probability, on top of the 0.60 base threshold. Exits keep 0.60, so being choosy about entering never makes a position harder to leave. `0` turns it off. |
| `--entry-confirmations` | `2` | An entry is acted on only when this many decisions in a row on the instrument chose the same BUY or SELL, each at or above the entry threshold. A HOLD, the other action, a weaker call, an approved entry or a new day starts the count again. `0` or `1` turns it off. |
| `--decide-every` | `5m` | How often an instrument is put to the decider, aligned to the clock (09:20, 09:25, ...). `1m` decides on every bar. Must be whole minutes that divide 30 (`1m`, `5m`, `15m`, `30m`, ...). |
| `--stop-pct` | `2` | Per-position stop, in percent of the average price. A long whose last price is this far below its average price is sold at once (a short is bought back this far above), however short its hold and whatever the decider says. `0` turns the fixed stop off. |
| `--stop-range-mult` | `0` | Widen the stop for a volatile stock: the stop distance becomes the larger of `--stop-pct` and this multiple of the stock's high-to-low range over the last 30 minutes. The distance is read when the position is opened and kept, so a falling price cannot widen its own stop. `0` (the default) uses the percentage alone. |
| `--max-hold` | `0` | Time exit: a position is sold once it has been held this long, counted from its first entry, whatever the decider says and however short the minimum hold. `0` means no time exit. |

With the defaults an entry needs two strong calls five minutes apart, so the earliest one is 09:25 for the first bar of the day (entries open at 09:20), Jev is asked a fifth as often, and a position is then held at least an hour (to the 15:10 square-off at the latest). What does not change: the stop, the time exit (when set), the square-off at 15:10, the daily loss limit and the kill switch exit at once on every tick, whatever the cadence, hold or cooldown; the capital budget, sizing and the rule that the bot only ever sells positions it opened are unchanged. The `momentum` baseline now reports 0.75 for its calls so it still clears the entry bar and stays a usable benchmark.

**The stop.** Before issue #59 the docs said a stop existed and none did, so a position the market turned on stayed held to the 60-minute minimum hold or beyond (the picker study had 6 of 57 positive-news stock-days lose more than 2% over the hour, the worst 5.0%). The stop is placed on the first mark at or through the level: if the price gaps over it, the sale is at the gapped price, not the level, and the paper broker fills it on the next tick like any order. 2% is a first choice, not a tested one; reports/0006 item 6 lists the study that would pick the level (`--stop-pct 0, 1, 1.5, 2, 3`). A stop trades the left tail for some whipsaw and may lower the mean, and it adds a sale and its costs to a position that would have recovered. A position restored from the journal has no entry record, so it gets the percentage stop but no time exit. A holding the bot did not open is never stopped.

In the walkthrough a stop or a time exit is an `EXIT` row, with the numbers in the gate text:

```text
09:31:30  EXIT   risk: stop_loss @ 1498.00 | gate stop_loss (last price 1498.00 is through the 1499.85 stop, 1.00% from the 1515.00 average price (1.12% adverse); the minimum hold does not apply) | pos +1 @ 1515.00  pnl -17.64
09:33:00  DECIDE SELL p=0.75 ... | gate reentry_cooldown (sold out of token 1 at 09:31:30, 2m0s ago; re-entry allowed after 10m0s) | pos flat  pnl -19.90
```

A time exit reads `EXIT   risk: time_exit @ ... | gate time_exit (held 10m0s since the entry at 09:21:30, the maximum is 10m0s; ...)`. The report's `Trading rules:` line says which stop and time exit the run used, and `RiskExits` counts them with the square-off and rotation sales.

To reproduce the old behaviour for a comparison: `--decide-every 1m --entry-confirmations 0 --entry-min-probability 0.6 --min-hold 5m --reentry-cooldown 10m --stop-pct 0`.

### Where the budget comes from

In this order, the first that is set wins:

1. `--capital <rupees>` on `run` and `replay`;
2. the **Key Vault secret `trading-capital`**, when `JEVTRADER_KEYVAULT` names a vault (locally through your `az login`; on the VM `prepare.sh` reads it with the VM's managed identity and puts it in the environment as `JEVTRADER_TRADING_CAPITAL`);
3. the environment variable `JEVTRADER_TRADING_CAPITAL`;
4. the built-in ₹25,000.

`run` logs `capital budget rupees=... source=... from=...` at start and `replay` prints the same on stderr. A value that is present but not a positive number (`lots`, `0`, `-5`) stops the command: it never silently falls back to the default. If the vault cannot be read, the environment variable or the default is used, with a warning.

`trading-capital` is a configuration value, not a credential: it is not redacted and is logged. To change it (the next start of `run` picks it up; a running session keeps the budget it started with):

```sh
az keyvault secret set --vault-name kv-jev-wpvd3louqq --name trading-capital --value 25000 --output none
az keyvault secret show --vault-name kv-jev-wpvd3louqq --name trading-capital --query value -o tsv   # check
```

You need *Key Vault Secrets Officer* on the vault to set it (the *Secrets User* role only reads). On the VM, restart the trading service after changing it: `sudo systemctl restart jevtrader-prepare jevtrader-run`. If the secret is not set, the VM boots with the default.

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

Flags for a session on one past day, used in the runbook below:

| Flag | Effect |
| --- | --- |
| `--start HH:MM`, `--end HH:MM` | trade only inside this IST window of each day (see [Session window](#session-window)) |
| `--walkthrough actions\|all` | add each run's chronological walk, instrument by instrument |
| `--compare` | add the comparison with what the market then did; also replays the `hold` and `momentum` baselines if they are not in `--deciders` |
| `--lookahead 5m,15m,30m` | with `--compare`, how long after each BUY/SELL to read the price |
| `--min-hold`, `--reentry-cooldown`, `--entry-min-probability`, `--entry-confirmations`, `--decide-every`, `--stop-pct`, `--stop-range-mult`, `--max-hold` | the trading rules; see [Trading for hours](#trading-for-hours) |
| `--index <token>` | name the index by instrument token (NIFTY 50 is `256265`) when the stored bars have no symbol |
| `--watchlist-file <file>` | replay only the instruments of a watchlist file written by `screen` (found in the stored data by symbol; the index stays as context; not with `--tokens`) |

Without these flags the output has the same shape as before, but the trading rules now default to trading for hours.

### Session window

`--start 09:20 --end 10:15` makes the engine decide only on bars that start at or after 09:20 and that end by 10:15. Bars of the day before `--start` are still fed through the same path, so the first decision already has real history: its returns, session range, VWAP and index context are built from 09:15 onwards, and the paper broker's marks are current. They are never decided on, so they cost no model calls and no orders. Data after `--end` never reaches the clock, the broker or the risk book; it only provides the prices the comparison looks at, so a call at 10:14 can still be checked 30 minutes later. A position still open at `--end` is marked at the window's last price, not closed.

The risk gate's own hours still apply inside the window: no new entries before 09:20 or from 15:00, and everything is squared off from 15:10. A decision at 09:16 therefore shows `gate outside_session`.

### Walkthrough

`--walkthrough all` adds, per run and per instrument, every step in time order (illustrative output):

```
Walkthrough: jev (s-jev)

  NSE:INFY: 55 decisions (BUY 2, SELL 1, HOLD 52), 3 fills, ends flat, net PnL +41.20
    -- 2026-10-05 --
    09:21:00  DECIDE BUY  p=0.71 size S @ 1515.00 | ret5 +1.00% ret15 n/a session +1.00% vwap +0.40% vol 1.20x range 1.00 index +0.30% | gate approved -> order BUY 6 | pos flat  pnl +0.00
    09:22:00  FILL   BUY 6 @ 1515.60 (costs 3.82) | pos +6 @ 1515.60  pnl -3.82
    09:41:00  DECIDE SELL p=0.64 size S @ 1531.00 | ret5 ... | gate approved -> order SELL 6 | pos +6 @ 1515.60  pnl +88.60
```

A decision row has the action, the probability and size the decider gave, the snapshot values that mattered (5 and 15 bar returns, session return, distance from VWAP, relative volume, position in the session range, the index's session return; `n/a` when there is too little history), the risk gate's verdict with its reason when it refused, the order the gate approved, and the running position and PnL (after costs, marked at the bar's close). A fill row has the side, quantity, price and costs. `actions` leaves out the HOLD rows (decisions the decider failed on stay), and the counts in the instrument's header still include them. In `--format json` the same rows are under `walkthrough`.

### Comparison with what the market did

`--compare` adds, from the replayed data, which includes the part of the day after `--end`:

- every BUY and SELL answer (refused ones too, with the gate's reason), with the actual price N minutes later for each `--lookahead`, at the window's end and at the day's close, and the percentage move in the direction the call bet on (positive means right). `n/a` means the data stops before that point. Per run and horizon, the number of calls that moved the called way and the mean move;
- a buy-and-hold benchmark: every instrument bought at the open of the window's first bar for about the size-M notional and held to the close of its last bar, after the same costs the paper broker charges;
- each run's net PnL over the window beside that benchmark, the always-HOLD baseline (which never trades) and the momentum baseline. Both baselines run on the same data, window and costs.

The benchmark buys at an open and the strategies buy at fill prices after decision latency, so they differ by that slippage. In `--format json` this is `comparison`.

## Runbook: paper-trade a past day locally

Goal: replay one past session, see which instruments Jev picks at the open, what it buys and sells and how the day flows, and compare that with what the market did. Everything runs on your Mac; no order reaches Kite.

### 0. Prerequisites

- **Azure CLI, logged in** (`az login`), with the *Key Vault Secrets User* role on the vault (the template gives the operator *Secrets Officer* when `operatorObjectId` is set; see [deploy/README.md](../deploy/README.md)).
- **A Kite Connect app** whose redirect URL is registered as in [docs/kite-auth.md](kite-auth.md) (`http://127.0.0.1:8765/kite/callback` by default).
- **A Kite historical-data subscription.** Kite's historical candles API (`backfill`) is part of the paid Kite Connect plan; on the free Personal plan Kite refuses the request and `backfill` fails with its permission error. Login, live ticks and orders do not need it, and neither does `replay` over data you recorded yourself with `record` (`--source ticks`). Check [Zerodha's current plans](https://kite.trade/) before relying on this.
- The Jev model is pinned to `jev-1.13.0` (`POST https://api.typesafe.ai/v1/systemone`); `replay` uses it for `--deciders jev`.

Build once: `go build -o jevtrader ./cmd/jevtrader` (or prefix the commands below with `go run ./cmd/jevtrader`).

### 1. Secrets from Key Vault

```sh
export JEVTRADER_KEYVAULT=kv-jev-wpvd3louqq
```

With it set, `login` reads `kite-api-key` and `kite-api-secret`, `backfill` reads `kite-api-key`, `replay` reads `jev-api-key` (only when `jev` is among the deciders), and `run` reads all three (`run` and `replay` also read the [`trading-capital` budget](#capital-budget), which is configuration rather than a secret), each with `az keyvault secret show --vault-name ... --name ... --query value -o tsv` through your `az login` session. Nothing is written to disk or to an env file, and a value is never printed or logged. If the vault cannot be read for a secret, the matching environment variable (`KITE_API_KEY`, `KITE_API_SECRET`, `JEV_API_KEY`) is used instead, with a warning that names the secret but not its value; if neither has it, the command fails and says what failed. Without `JEVTRADER_KEYVAULT` the environment variables are the only source, as before. `record` still reads `KITE_API_KEY` from the environment.

### 2. Log in to Kite

```sh
./jevtrader login
```

It prints a Kite login URL; open it in your browser on the Mac, log in and complete 2FA. The redirect lands on the listener on `127.0.0.1:8765` directly, so no tunnel is needed (that is only for a remote VM). The session is saved under `~/.local/state/jevtrader/` (mode `0600`) and is valid until 06:00 IST the next day; `backfill` picks it up from there, so you do not export `KITE_ACCESS_TOKEN`.

### 3. Backfill the day

```sh
./jevtrader backfill --symbols "NSE:INFY,NSE:TCS,NSE:RELIANCE,NSE:NIFTY 50" --from 2026-09-30 --derive ""
```

This fetches 1-minute candles for each instrument into `data/bars_backfill/` (about 375 per instrument for a full session). `--symbols` looks the instruments up in Kite's instrument master (cached under `--data-dir`) so the stored bars carry their symbol; `--tokens 408065,256265` also works but leaves the bars without one. Always include `NSE:NIFTY 50`: it is the index context in every snapshot and is never traded. Backfill refuses a day it already holds; delete `data/bars_backfill/date=2026-09-30` to fetch it again. It makes 3 requests a second at most.

### 4. Replay a window

```sh
./jevtrader replay --source backfill --from 2026-09-30 --to 2026-09-30 \
  --start 09:15 --end 10:15 --deciders jev \
  --walkthrough all --compare --run-prefix d0930 --out replay-2026-09-30.txt
```

With the default `--decide-every 5m` Jev is asked once per instrument per five minutes, so this hour is about 12 calls per instrument (about 60 with `--decide-every 1m`); start with two or three instruments. The first decision comes at 09:20 on the bar that closes then, when the risk gate allows entries, and the entry needs two strong calls in a row (`gate entry_unconfirmed` on the first). To give the decider an hour of warm-up context without trading it, use `--start 09:45 --end 10:45`; the bars from 09:15 still build the snapshots. Add `--format json` for the machine-readable report, `--walkthrough actions` to hide the HOLD rows, `--lookahead 10m,30m,60m` to change how far ahead the prices are read. `--compare` also replays the baselines, so `hold` and `momentum` appear in the table without being named. Every run is also written to the decision log (`--log`, default `state/decisions.db`), which can be queried or replayed with `recorded:<run-id>`.

### 5. Read the report

Top to bottom:

1. **Evaluation report**: one row per run (net PnL after costs, trades, hit rate, drawdown, turnover, B/S/H counts) and the calibration table. The line under the title names the window.
2. **Walkthrough**: the opening decisions per instrument show what Jev saw (returns, VWAP distance, volume, index) and what it answered; later rows show what it bought and sold, the price each fill got and how position and PnL moved. Look at the first rows of each instrument for the leads at the open, and at `gate ...` for what the risk gate refused and why.
3. **Comparison with the real market**: the price after each BUY and SELL at the lookahead horizons, at the window end and at the close, with the move in the called direction; then the buy-and-hold benchmark per instrument, and a table with each run's net PnL against buy-and-hold, always-HOLD and momentum.

A replay of a past day is an optimistic upper bound: Jev may have seen that period in training, and the replay fills orders on minute candles, not on the order book. Treat it as a way to see how the system behaves, not as proof of an edge.

### Troubleshooting

| Symptom | Cause |
| --- | --- |
| `key vault kv-...: az failed: ... Forbidden` | your identity lacks *Key Vault Secrets User* on the vault |
| `key vault ...: the az CLI is not installed` / `Please run 'az login'` | install the Azure CLI and `az login`; or unset `JEVTRADER_KEYVAULT` and export the variables |
| `backfill`: `Insufficient permission` or an HTTP 403 on `/instruments/historical` | the Kite app has no historical-data subscription |
| `KITE_API_KEY and KITE_ACCESS_TOKEN must be set` | run `jevtrader login` (the saved session is missing or expired) |
| `index "NSE:NIFTY 50" is not in the stored data` | backfill without `NSE:NIFTY 50` or without `--symbols`; add it, or pass `--index 256265` |
| every decision is a HOLD with `[decider failed: ...]` | Jev was unreachable or returned an invalid answer; check `JEV_API_KEY`/the vault secret and `--decide-timeout` |

## Decision log

`internal/decisionlog` is append-only (triggers reject `UPDATE` and `DELETE`).
Tables: `runs`, `cycles` (snapshot, answer, risk verdict, intent; written
before the order is submitted), `order_events` (one per broker update),
`equity`, and the two [shadow tables](#the-shadow-log-and-jevtrader-shadow-report),
`shadow_signals` and `shadow_outcomes`. Open it with any SQLite client; WAL
mode allows reading while a run is writing.

The schema version is in `PRAGMA user_version` (now 2). A log written at
version 1 opens with this binary and is migrated in one transaction: the two
shadow tables are added and nothing already in it is touched. A binary older
than the migration refuses a migrated log rather than guess, so roll the log
back from the Blob Storage backup if the image is rolled back.

## The shadow log and `jevtrader shadow-report`

The capital budget (₹25,000) decides which signals the bot can act on, and it
cannot resolve an edge of the size at stake in under a year. Per-event returns
can, so `run` also logs **every candidate signal**, whether or not it was
traded, and the report reads what the market did after each. Nothing in the
shadow log can change what is decided or ordered: it only writes, a failed
write is logged and counted (`ShadowErrors` in the engine's stats) and the run
goes on, and `replay` and `study` do not write it.

What is logged, in `shadow_signals`:

| Signal | When | Fields |
| --- | --- | --- |
| `pick` | the first decision bar of the day, once per picker entry in the watchlist file | rank, score, the screen's signals and the most material news item's Jev scores (impact, material, direction), as JSON in `detail`; counts from the first moment the gate allows an entry (09:20 by default) |
| `decision:BUY`, `decision:SELL`, `decision:HOLD` | every decider answer, beside its `cycles` row | the answer's probability, the risk gate's verdict and detail (`approved`, `below_threshold`, `short_not_allowed`, ...) and whether an order was approved (`traded`) |
| `decision:FAILED` | a decider error or timeout, which becomes a HOLD | as above; it is not an answer, so it has its own label |

Each signal carries its time, the price when known, the end of the trading
window (where entries stop, 15:00 by default) and the index the run watched. A
signal for the same day, kind, instrument and time is logged once, so a restart
the same day does not repeat the picks.

**Outcomes** are one `shadow_outcomes` row per signal and horizon, appended
later from the recorded bars (`--data-dir`, live `bars` first, then
`bars_backfill`): the stock's move from the signal's price to the close of the
last bar at or before **+15 minutes, +60 minutes, the window end and the day
close (15:30)**, with the index's move and the mean move of the day's screened
universe over the same interval. A horizon past the day's close, or a window end
that is not after the signal, does not apply. An illiquid stock keeps its last
price until the target. A horizon the recorded bars do not reach yet is left
for a later fill, so a backfill of a gap completes it.

```
jevtrader shadow-report --from 2026-10-05 --to 2026-10-30
jevtrader shadow-report --from 2026-10-05 --to 2026-10-30 --benchmark universe --by-verdict --json
```

The command first writes the outcomes the bars now answer (`--fill=false` to
only read), then prints, per signal and horizon:

- **N** and **DAYS**: outcomes and the distinct trading days among them.
- **MEAN %**: the mean **market-neutral** return in the direction the signal bet on, in percent: the stock's move minus the benchmark's, times +1 for a pick, a BUY and a HOLD (read as a rise) and −1 for a SELL. Positive means the signal was right on average.
- **CI95 %**: the 95% interval from resampling **whole days** with replacement (2,000 resamples, `--resamples`, repeatable with `--seed`). Outcomes of one day share that day's market, so the day is the unit; with fewer than two days there is no interval.
- **HIT %**: the share of outcomes above zero.

Each decision signal is also split into `traded` and `not traded`
(`--by-verdict` adds one row per gate verdict), and the header counts the
outcomes still awaiting bars and those left out for want of a benchmark.

`--benchmark` picks what a return is netted against:

- `index` (default): the index the run watched over the same interval, `NSE:NIFTY 50` unless `run --index` says otherwise. Midcap 150 and Smallcap 250, which are what the picks come from, are not streamed by `run`, so they are not available.
- `universe`: the mean move of every stock the day's signals were about, as recorded. This is the picker's own cross-section, so over all of a day's picks it is zero by construction; it separates signals from each other (a BUY from a HOLD), not from the picker's list.
- `none`: the raw move.

Run it weekly over the forward days only, and set it beside the historical
result of `jevtrader study`. The VM stores the log and the bars, so any
machine with a copy of both can run it.


## Runbook: screen, then paper-trade the screened list locally

1. `export JEVTRADER_KEYVAULT=kv-jev-wpvd3louqq` and `./jevtrader login` as above.
2. Before the open, `./jevtrader screen` (at about 08:30 IST it is the early pass; run it again after 09:08 for the final pass with the pre-open gap). Read the summary: each entry lists why it is there.
3. `./jevtrader run --watchlist-file state/watchlist/$(TZ=Asia/Kolkata date +%F).json --watchlist NSE:INFY,NSE:TCS` — the second list is the fallback.

To see what the screen would have picked on a past day and how that day went (RSS history does not exist, so the news signal then rests on NSE filings alone):

```sh
./jevtrader screen --date 2026-10-01
./jevtrader backfill --symbols "$(jq -r '[.entries[].key] | join(",")' state/watchlist/2026-10-01.json),NSE:NIFTY 50" --from 2026-10-01 --derive ""
./jevtrader replay --source backfill --from 2026-10-01 --to 2026-10-01 --start 09:15 --end 10:15 \
  --watchlist-file state/watchlist/2026-10-01.json --deciders jev --compare --out replay-screened-2026-10-01.txt
```
