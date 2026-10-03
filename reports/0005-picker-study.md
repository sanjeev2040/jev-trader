# 0005 — Backtesting the picker over several weeks

- **Status:** Proposed (the command is implemented in the PR that closes #47; no real multi-week result is recorded here yet)
- **Date:** 2026-10-03
- **Issue:** [#47](https://github.com/sanjeev2040/jev-trader/issues/47)
- **Depends on:** [0004 — Pre-market watchlist](0004-watchlist.md), [0002 — System architecture](0002-architecture.md)
- **Decider:** repository owner

## Context

The one-day replay of the picker's fifty mid and small caps on 2026-10-01 (09:15–10:15, a budget of ₹25,000) had buy-and-hold of the picks at −615 and Jev at −224. One day cannot say whether the picker's weights (news 0.40, sector 0.25, gap 0.25, range 0.10) pick anything, or whether Jev adds anything to what the market did. This report fixes how the picker is backtested over several weeks. How to run it is in [docs/picker-study.md](../docs/picker-study.md).

## Decision

A command, `jevtrader study --from … --to …` (or `--weeks N`), composes the commands that already exist instead of reimplementing them. For each NSE trading day it:

1. runs the screen as of that morning (`screen --date`: historic phase, NSE filings and daily candles public before the open), keeping the whole screened pool with its scores and signals (`watchlist.File.Pool`, written only when asked for);
2. draws a random list from the pool, without the picks and by default of the same size, seeded so a rerun draws the same one;
3. backfills 1-minute candles of both lists and the index, per instrument and day, fetching only what a manifest says is missing;
4. replays the window with the real engine, risk gate, capital budget and paper broker: the picks with Jev, the always-HOLD and momentum baselines, and the random list with momentum, each with the market comparison;
5. stores the day's result.

`study.Summarize` and `study.WriteMarkdown` aggregate the stored results into the report: per-day and total net P&L against buy-and-hold of the picks and of the random list, with the day-by-day differences and their `t`; hit rates (the share of names that rose, Jev's round-trip hit rate and the share of its BUY/SELL answers that moved the called way); calibration of Jev's probability, merged over days; per-signal Pearson and Spearman correlation, daily rank correlation and above/below-zero means against each stock's window return; and Jev's token cost with its list price.

### Why the random list is drawn from the screened pool

The question the picker has to answer is whether its ranking beats picking at random among stocks that were equally tradeable, so the comparison list comes from the stocks that passed the same universe and liquidity filters that morning, not from a convenient fixed list. It is the same size as the picks, so buy-and-hold P&L is comparable; the report also gives the mean return per name. The pool is recorded in the watchlist file only when the screen is asked to keep it (`Config.KeepPool`), so the file `run` reads is unchanged.

The stocks of the random list are also what makes the signal table informative: the picks are a selection from the pool by the score, so within the picks alone the score and its signals have little spread. The report gives the signal statistics for the picks and for picks plus random list together.

### Cost control

Everything is cached under `--data-dir` and a rerun asks nothing it has been told before:

- **Daily candles** for the screen are fetched once per stock for the whole range (`study.NewRangeMarket`) and sliced for each day; the screen's own per-day candle cache and its news-score cache still apply.
- **Minute candles** are stored per instrument and day under `<data-dir>/study/bars_backfill`, with a manifest per day, so a changed random list fetches only the new names. An instrument that came back empty is not recorded and is asked again on the next run, in case the cause was temporary. Kite is called at no more than three requests a second.
- **Jev's answers** are stored by model and exact snapshot (`study.AnswerStore`, wrapped around the Jev client as a decider). Reruns, resumed runs and weight experiments that leave a snapshot unchanged are free, and a replay is deterministic. A failed call is never stored. A fixed decision latency (`--latency`, default 1 s) makes the fills independent of how fast Jev answered.
- **Day results** are stored under a key of everything they depend on; a changed setting recomputes the day, and `--report-only` rebuilds the report without calling anything.

The report counts Jev's tokens paid for, tokens answered from the store, and the news-scoring tokens of the screens, at the list price of $42 per billion input tokens.

### Validity checks the report makes visible

- HOLDs that stand in for a failed Jev call are counted per decider and flagged when there are any, since they understate Jev.
- Skipped days (closed, no candles, no screen result) are listed with their reasons.
- Missing instruments (no instrument in today's master, no candles) are dropped from a day's list and recorded in its result.

## Limits, which the report repeats

- **Index constituents are today's.** NSE's files cannot be rewound, so the universe and the ASM/GSM lists of a past day are today's, and delisted stocks are absent from Kite's instrument master (survivorship).
- **No pre-open gap** on a past day, so the gap signal (weight 0.25) and the sectoral-index move are absent and their weights are spread over the rest. The live picker's final pass sees more.
- **News is NSE filings only**; the RSS feeds keep no history.
- **Jev may have seen the period**, so its numbers are an upper bound. The picker's own signals are not affected.
- **Minute candles**, not the order book.
- **A handful of weeks**, and the stocks of a day share the market's move: the `t` statistics treat days as independent and flatter.

## Consequences

- The answer to "are the picker's weights proven" is a report, not an opinion; the weights stay priors until a study over weeks (and later forward paper trading) supports them. The study can compare weight sets cheaply: a different `--w-*` re-screens from cached candles, and only unseen snapshots cost tokens.
- A real multi-week run needs the operator's Kite session and Jev key and takes an hour or two, so it is run by hand and its report committed (or linked) afterwards; CI runs the study's tests on fixtures only.
- Past-day results can justify keeping or dropping a signal for a forward test. They are not a tuning set for the weights: with a few weeks of correlated stocks, fitting them would overfit.
