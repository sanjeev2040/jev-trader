# Backtesting the picker (`study`)

`jevtrader study` runs the pre-market picker over many past trading days and answers the questions one replay cannot: does the picked list beat a random list of the same size, does Jev add anything to buy-and-hold of the picks, is Jev's probability honest, and which of the picker's signals went with the morning's returns. The design and its limits are in [reports/0005-picker-study.md](../reports/0005-picker-study.md).

```sh
export JEVTRADER_KEYVAULT=kv-jev-wpvd3louqq
./jevtrader login
./jevtrader study --weeks 5 --data-dir data --out reports/picker-study-2026-10.md
```

It needs a Kite session with the historical-data plan and a Jev key, like `screen` and `replay` (see [run-and-replay.md](run-and-replay.md#runbook-paper-trade-a-past-day-locally)). It is a command you run on your Mac; CI does not run it, and its tests use fixtures only.

## What it does for each trading day

1. **Screen as of that morning**: `screen --date` for the day, in the historic phase: the constituent lists, the daily candles up to the previous session and the NSE filings public before the open. The screened pool (every stock that passed the liquidity filters, with its score and signals) is kept with the picks.
2. **Choose the comparison list**: a random list, by default as many stocks as the day's picks, drawn from the same pool without the picks (`--random-size`, `--seed`, or `--no-random`). The same seed, day and pool give the same list.
3. **Backfill** the 1-minute candles of the picks, the random list and `NSE:NIFTY 50` (the index context).
4. **Replay the window** (`--start 09:15 --end 10:15`) over the picks with `--deciders` (default `jev,hold,momentum`) and over the random list with `--random-deciders` (default `momentum`; buy-and-hold of the random list is always read). It is the same engine, risk gate, capital budget and paper broker as `replay`, with the market comparison on.
5. **Store the day's result.**

Then it aggregates over the days and writes the report.

Days the exchange is closed are skipped using `deploy/nse-holidays.yaml` (`--calendar ""` takes every weekday and skips a day Kite has no candles for). A day whose screen finds no list, or for which Kite has no candles, is reported as skipped with the reason.

## The report

A Markdown file (`--out`, default `<data-dir>/study/report-<from>_<to>.md`) and the same data as JSON beside it (`.json`). Top to bottom:

| Section | What it answers |
| --- | --- |
| Results | total and mean-per-day net P&L after costs, positive days, trades and round-trip hit rate for buy-and-hold of the picks, of the random list, and each decider |
| Against buy-and-hold and against a random list | day-by-day differences (picks − random; Jev − picks buy-and-hold; Jev − momentum; …) with their spread, `t` and how many days they were ahead |
| Hit rates | the share of picked and of random stocks that rose over the window; the share of Jev's (and momentum's) BUY and SELL answers that moved the called way, at each horizon |
| Per day | the day-by-day table behind the totals |
| Calibration | Jev's stated probability against how often the price then moved that way, merged over all days, and its Brier score |
| Which signals went with the window return | sector, news, range and the score itself against each stock's window return: Pearson and Spearman over all stock-days, the mean daily rank correlation (information coefficient) with its t, and the mean return where the signal was above zero, below zero, or zero/not read; for the picks alone and for picks plus the random list |
| Jev token cost | tokens and list price of the decisions paid for, of the decisions answered from the store (what they saved), and of the news scoring in the screens |
| Skipped days, Limitations | what was left out, and why a past-day study is not the live picker |

The report counts HOLDs that stand in for a failed Jev call (timeout, rate limit, invalid answer) and warns when there are any: those days understate Jev. Run with `--refresh` to ask again; answers already stored are not paid for twice.

## What is cached, and what a rerun costs

Everything is under `--data-dir`, so a rerun, a resumed run after a failure and a rerun with a changed setting ask Kite and Jev only for what is new:

| What | Where | Reused when |
| --- | --- | --- |
| Instrument master, constituent lists | `<data-dir>/` (as `screen`) | the same day |
| Daily candles of the universe | `<data-dir>/screen/` (as `screen`), fetched once per stock for the whole range | always, for a day already screened |
| News scores from Jev | `<data-dir>/screen/news-<date>.json` (as `screen`) | the same pair of news item and company under the same model |
| Watchlists with their pool | `<data-dir>/study/watchlist/<screen key>/<date>.json` | the picker settings (top N, weights, sector cap, minimum score, news sources, model) are unchanged |
| 1-minute candles | `<data-dir>/study/bars_backfill/` and `<data-dir>/study/fetched/<date>.json` | per instrument and day, whatever else changes |
| Jev's answers | `<data-dir>/study/jev-answers.jsonl` | the same model asked about the same snapshot; a failed call is never stored |
| Day results | `<data-dir>/study/days/<run key>/<date>.json` | the window, deciders, random-list settings, capital, latency, horizon and picker settings are unchanged |
| The decision log of the replays | `<data-dir>/study/decisions.db` | for inspection; every replay is a run `study-<date>-<list>-<time>-<decider>` |

So changing the weights re-screens (daily candles are cached) and replays, and only the snapshots Jev has not seen before cost anything. `--report-only` calls nothing and rebuilds the report from the stored day results.

Kite is called at three requests a second, as `screen` and `backfill` do, and at most that for the whole study. For a five-week study (about 25 trading days) expect, roughly:

- about 390 daily-candle requests once (about two minutes), and about 100 minute-candle requests a day (about 35 seconds a day, about 15 minutes in all);
- about 50 stocks × 60 minutes = 3,000 Jev decisions a day, at most eight in flight, so some minutes a day and an hour or two for the study. The token cost is in the report; the first day's report is the measure to extrapolate from, at $42 per billion input tokens.

## Flags

`jevtrader study --help` lists them all. The ones that matter:

| Flag | Default | |
| --- | --- | --- |
| `--from`, `--to`, `--weeks` | `--to` yesterday | the range: `--from YYYY-MM-DD`, or `--weeks N` ending at `--to` |
| `--start`, `--end` | `09:15`, `10:15` | the window each day is traded and held over |
| `--deciders` | `jev,hold,momentum` | deciders over the picks |
| `--random-deciders`, `--random-size`, `--no-random`, `--seed` | `momentum`, the picks' size, off, `1` | the random list; `jev` in `--random-deciders` doubles the token cost |
| `--top`, `--w-news`, `--w-sector`, `--w-gap`, `--w-range`, `--max-per-sector`, `--min-score`, `--news-sources` | as `screen` | the picker's settings; the liquidity filters are `screen`'s defaults |
| `--capital` | Key Vault `trading-capital`, then the environment, then ₹25,000 | the budget the deciders trade with (see [Capital budget](run-and-replay.md#capital-budget)) |
| `--latency`, `--decide-timeout`, `--horizon`, `--lookahead` | `1s`, `20s`, `5`, `5m,15m,30m` | as `replay`; a fixed latency keeps a result from depending on how fast Jev answered |
| `--refresh`, `--report-only` | | recompute stored day results; or call nothing and only rebuild the report |
| `--out`, `--data-dir`, `--calendar`, `--log` | | where things go |

## Limits of a past-day study

Printed in every report: the index constituents and the ASM/GSM lists are today's; there is no pre-open gap on a past day (the gap signal, and the sectoral-index move, are absent and their weights are spread over the rest); the news is NSE filings only because the RSS feeds keep no history; Kite's instrument master is today's, so delisted stocks cannot be studied (survivorship); Jev may have seen the period, so its result is an upper bound; the fills are on minute candles; and a handful of weeks is a small sample whose stocks share each day's market move. Read the signal table as a first look at which signals deserve a forward test, not as a tuning set.
