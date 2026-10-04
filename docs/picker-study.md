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
3. **Backfill** the 1-minute candles of the picks, the random list and `NSE:NIFTY 50` (the index context); with `--universe`, of every other stock of the screened pool too, and read what each did over the window straight from the stored candles (no replay, no decider).
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
| Market-neutral evidence | the picks, the random list and each news bucket (news above 0, below 0, scored but net zero, no scored item) as **excess** over the day's cross-section at the same window, with the pooled mean, a 95% interval that resamples days, and how many calendar weeks were positive; the day-by-day spread of positive against negative news; a week-by-week table. See [Market-neutral evidence](#market-neutral-evidence) |
| Which signals went with the window return | sector, news, range and the score itself against each stock's window return: Pearson and Spearman over all stock-days, the mean daily rank correlation (information coefficient) with its t and its day-resampled 95% interval, and the mean return where the signal was above zero, below zero, or zero/not read; for the picks alone and for every stock measured (the picks and the random list, or the whole pool with `--universe`) |
| Jev token cost | tokens and list price of the decisions paid for, of the decisions answered from the store (what they saved), and of the news scoring in the screens |
| Skipped days, Limitations | what was left out, and why a past-day study is not the live picker |

The report counts HOLDs that stand in for a failed Jev call (timeout, rate limit, invalid answer) and warns when there are any: those days understate Jev. Run with `--refresh` to ask again; answers already stored are not paid for twice.

## Market-neutral evidence

A stock's raw return over the window is mostly the market's day. The report therefore leads with the **excess** return: the stock's return minus the mean of its day's cross-section over the same window (the same minutes). The cross-section is, in order of preference, the whole screened pool (`--universe`), else the random list, else the picks and the random list together; the report says which it used on how many days, and leaves out a day with fewer than three stocks.

- **Pooled mean and interval.** The mean pools stock-days. The 95% interval resamples *days* with replacement (2,000 seeded draws, so a report is reproducible), because the stocks of one day share the market and are not independent observations. It needs at least three days.
- **Weeks positive.** The calendar weeks (Monday to Sunday) whose pooled mean excess was above zero, out of the weeks with data: whether an effect is in most weeks or in a few. The week-by-week table gives each week's mean for the picks, the stocks with news above zero and those below.
- **The news table.** Every stock measured, grouped by the sign of its net news signal that morning: above 0, below 0, scored but net zero, and no scored item. With `--universe` that is every screened stock-day, not only the picks, and negative news is included (a day on which no stock has a catalyst still has its pool measured). The last row, *news above 0 − news below 0*, is the day-by-day difference of the two groups' mean excess over the days that had both.
- **After costs.** The *Long, excess after costs* column is the mean excess less one round trip (charges at the size-M notional plus the paper broker's slippage on both sides, about 0.15% at ₹2,500).

```sh
# The universe-wide news evidence over 26 weeks (hours of Kite requests the first time; see below).
./jevtrader study --from 2026-04-01 --to 2026-10-01 --start 09:20 --end 10:20 \
  --universe --max-news 600 --deciders hold --no-random --out reports/news-universe.md
```

The picker is the live one (a positive catalyst is required, so the picks are the catalyst names); `--universe` adds the measurement of every other screened stock, so the news table has the picks, the negative-news stocks and the large unscored remainder. `--deciders hold --no-random` makes it a pure measurement: no decision tokens, and the pool itself is the benchmark. Without `--universe` the news table covers only the picks and the random list, and says so.

## The look-ahead control

A model that was trained on the period a study replays can recall what happened to a named company on a named date instead of judging the news. TypeSafe publishes no training cutoff for jev-1.13.0, so the historical news result is unproven until a control says it does not depend on that. `--anonymize-news` (on `study`, and on `screen`) hides what would let it:

- the company's name (the registered name, the shorter ways news writes it, and each distinctive word of it) and its symbol become "the company", in the headline, the details and the filing category;
- every date in the text (`2026-10-04`, `4 October 2026`, `Oct 4`, `04/10/2026`, years, `FY27`, quarters, month and weekday names) becomes a placeholder, and the publication time is not sent at all; a number that is an amount (`Rs 2000 crore`) is left alone;
- the sector, the source and the wording stay, and so does the filing category (with any company name or date in it hidden).

Run the same range twice, with and without it, and compare the news tables:

```sh
./jevtrader study --from 2026-04-01 --to 2026-10-01 --start 09:20 --end 10:20 --universe --max-news 600 \
  --deciders hold --no-random --out reports/news-named.md
./jevtrader study  ...the same flags... --anonymize-news --out reports/news-anonymised.md
```

If the positive-news excess survives anonymisation (its interval still above zero, and about as large) the look-ahead worry is much smaller; if it halves or loses its interval, the historical number is partly recall and only forward results count. The two runs keep separate watchlists and day results (the anonymised flag is part of the screen key), and Jev's answers are cached under the exact request, so the named and the anonymised scores never mix and each is asked once. Two companies whose items read the same once the name is hidden are one request. Anonymising removes the obvious identifiers; it cannot remove a famous event the wording itself gives away.

## What is cached, and what a rerun costs

Everything is under `--data-dir`, so a rerun, a resumed run after a failure and a rerun with a changed setting ask Kite and Jev only for what is new:

| What | Where | Reused when |
| --- | --- | --- |
| Instrument master, constituent lists | `<data-dir>/` (as `screen`) | the same day |
| Daily candles of the universe | `<data-dir>/screen/` (as `screen`), fetched once per stock for the whole range | always, for a day already screened |
| News scores from Jev | `<data-dir>/screen/news-<date>.json` (as `screen`) | the exact request: the same model, the same state sent about the news item (named or anonymised) and the same questions |
| Watchlists with their pool | `<data-dir>/study/watchlist/<screen key>/<date>.json` | the picker settings (top N, weights, sector cap, minimum score, news sources, model) are unchanged |
| 1-minute candles | `<data-dir>/study/bars_backfill/` and `<data-dir>/study/fetched/<date>.json` | per instrument and day, whatever else changes; an instrument that returned no candles is asked again |
| Jev's answers | `<data-dir>/study/jev-answers.jsonl` | the same model asked about the same snapshot (the exact request); a failed call is never stored |
| Day results | `<data-dir>/study/days/<run key>/<date>.json` | the window, deciders, random-list settings, `--universe`, capital, latency, horizon and picker settings (including `--anonymize-news` and `--max-news`) are unchanged |
| The decision log of the replays | `<data-dir>/study/decisions.db` | for inspection; every replay is a run `study-<date>-<list>-<time>-<decider>` |

So changing the weights re-screens (daily candles are cached) and replays, and only the snapshots Jev has not seen before cost anything. `--report-only` calls nothing and rebuilds the report from the stored day results.

### Long ranges (26 weeks)

A range of months is made to be run in pieces and resumed:

- **Resumable.** Each day is stored when it is done (a skipped day is tried again next time), the 1-minute candles are noted in a manifest after every batch of 25 stocks, and Jev's answers are stored as they come. Stop the run (Ctrl-C), lose the connection or let Kite's session expire, run the same command again, and it carries on from the first day not stored and the first stock not fetched. A stopped run still writes a report of the days it has (`--report-only` rebuilds it without calling anything).
- **Each thing is fetched once.** Daily candles come with one request per stock for the whole range; minute candles once per stock and day, whatever the run key.
- **Rate limits.** Kite is held to three historical requests a second. A request that fails with a rate-limit answer, a gateway timeout or a dropped connection is tried again up to four times with a doubling wait; an expired session or a missing permission stops the run at once. Jev calls (the news scoring and the decisions that are not answered from the store) are held to `--jev-rate` a second (default 10, 0 for no limit); the client itself retries a 429 or 529 with backoff.
- **Progress.** The run logs each day as `study day n/N`, so a long run shows how far it is.

Roughly, for 26 weeks (about 130 trading days): the picks-only study is the five-week estimate below scaled up; `--universe` fetches about 390 minute-candle series a day instead of about 100, so about 2.5 minutes a day on Kite's limit, or about five hours for the range the first time. Start with a two-week range to check the depth Kite returns and the pace.

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
| `--top`, `--w-news`, `--w-sector`, `--w-gap`, `--w-range`, `--require-catalyst`, `--catalyst-material`, `--catalyst-direction`, `--catalyst-impact`, `--max-per-sector`, `--min-score`, `--news-sources` | as `screen` (news-led weights and a required catalyst; a day without a catalyst has no picks) | the picker's settings; the liquidity filters are `screen`'s defaults |
| `--capital` | Key Vault `trading-capital`, then the environment, then ₹25,000 | the budget the deciders trade with (see [Capital budget](run-and-replay.md#capital-budget)) |
| `--latency`, `--decide-timeout`, `--horizon`, `--lookahead` | `1s`, `20s`, `5`, `5m,15m,30m` | as `replay`; a fixed latency keeps a result from depending on how fast Jev answered |
| `--universe` | off | measure every stock of the screened pool each day (hold only) for the market-neutral benchmark and the universe-wide news table; fetches the whole pool's minute candles |
| `--anonymize-news` | off | the look-ahead control: hide the company, the symbol and the dates from the news text sent to Jev |
| `--max-news` | `150` | most (news item, company) pairs Jev scores a day; raise it (600 or more) with `--universe` |
| `--jev-rate` | `10` | most Jev calls a second; `0` for no limit |
| `--refresh`, `--report-only` | | recompute stored day results; or call nothing and only rebuild the report |
| `--out`, `--data-dir`, `--calendar`, `--log` | | where things go |

## Limits of a past-day study

Printed in every report: the index constituents and the ASM/GSM lists are today's; there is no pre-open gap on a past day (the gap signal, and the sectoral-index move, are absent and their weights are spread over the rest); the news is NSE filings only because the RSS feeds keep no history; Kite's instrument master is today's, so delisted stocks cannot be studied (survivorship); Jev may have seen the period, so its result is an upper bound (`--anonymize-news` tests how much the news score depends on that); the fills are on minute candles; and a handful of weeks is a small sample whose stocks share each day's market move. Read the signal table as a first look at which signals deserve a forward test, not as a tuning set.
