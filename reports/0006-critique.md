# 0006 — Critique of jev-trader: why it loses, and a ranked list of testable improvements

- **Status:** Proposed (a critique and a work list; no code is changed by this report, and none of the proposed changes has been run)
- **Date:** 2026-10-04
- **Issue:** [#58](https://github.com/sanjeev2040/jev-trader/issues/58)
- **Depends on:** [0002 — System architecture](0002-architecture.md), [0004 — Pre-market watchlist](0004-watchlist.md), [0005 — Backtesting the picker](0005-picker-study.md)
- **Decider:** repository owner

## Context

The captain's ask: the product is good but it mostly loses; perhaps some context is missing when the state is given to Jev; have an agent go through web research and the repository, critique the design and propose improvements.

This report does three things: it reads exactly what Jev is sent and returns and judges that against TypeSafe's own guidance; it re-analyses the local study data (the decision log and the cached candles from the 2026-09-14 to 2026-10-01 study, no Jev or Kite call) to test what Jev's answers carry; and it collects outside evidence on intraday trading in Indian small and mid caps. It ends in a ranked list of changes, each with the `jevtrader study` run that would confirm or reject it.

## Summary

**The captain's hunch is half right, and the half that is wrong matters more.** Context is missing, but adding more of the same kind of context would not turn the losses into profit. The evidence, in order of weight:

1. **Jev's per-bar BUY/SELL call is mostly a function of the numbers it is given, and those numbers carry no tradable information at 1–60 minutes in this universe.** On the 13,590 decisions that have every feature, a linear fit on 12 of the snapshot's numbers (all the stock's own features, the index's 5-bar and session returns, and the clock) reproduces 57% of the variance of Jev's `P(BUY) − P(SELL)` (VWAP distance alone has rank correlation 0.65 with it). Against what the stock did next, market-neutral, Jev's score has a rank correlation of **−0.05 at 5 minutes (t −2.6 over 10 days)**, and every price feature in the snapshot is slightly negative too. The market-neutral spread between the top and bottom decile of any price feature is at most about 10 basis points at 5–30 minutes (one reversal leg reaches 16, about the cost); the round trip costs about 15. See [What Jev's answers carry](#what-jevs-answers-carry).
2. **Costs set a bar this task cannot clear.** A round trip costs about 0.15% (charges 0.10% plus 2 basis points of modelled slippage a side). The typical market-neutral move of one of these stocks over 5 minutes is 0.13%, over an hour 0.43%. To break even a symmetric strategy would need to be right 105% of the time at 5 minutes, 73% at 30, 67% at an hour. Jev was right 42–48%. About 60% of Jev's loss in the baseline study was costs (₹852 of ₹1,435), the rest was a negative gross result (−₹583).
3. **Jev's probabilities are not market probabilities.** TypeSafe trains them against its own outcomes; its documentation says calibration is "measured across groups of predictions" and nowhere claims it for stock direction. In the log, calls at 0.9–1.0 moved the called way 46.8% of the time. The risk gate's probability thresholds (0.60, 0.70) and its cost-edge test (which reads the probability as a win rate) therefore filter on how peaked the answer is, not on how likely it is to be right.
4. **The one signal that worked, news, is the one place where Jev is used as designed (text in, typed judgement out), and it is not in the decision path.** The news result is real but thin: 57 positive-news stock-days over 13 days, a market-neutral mean of +0.29% over the first hour (95% CI by day-resampling +0.06% to +0.54%) and nothing measurable by the close (+0.21%, CI −0.24% to +0.71%). The news-led picker has since produced 23 stock-days in 12 days (1.9 a day), which cannot tell edge from luck.
5. **At ₹25,000 and about two names a day, even a real edge would earn pocket change and could not be detected for a year.** An edge of 0.14% net on ₹2,500–5,000 a name is ₹7–13 a day against a day-to-day noise of about ₹55. Detecting it at 80% power needs about 240 trading days.
6. **There is no stop**, although the docs say there is one, and since #53 a decider cannot exit a position for an hour.

**The recommendation in one sentence:** stop asking Jev to forecast the next minutes from price numbers; trade a news-catalyst basket on fixed rules (with a stop), use Jev only for the text judgements it is built for, and judge every change on per-event, market-neutral, net-of-cost returns over at least 100 trading days, with look-ahead and cost-realism tests built in.

The ranked list in full (details, runs and rejection tests in [Ranked improvements](#ranked-improvements)):

| # | Change | Expected effect | Effort |
| --- | --- | --- | --- |
| 1 | Trade the catalyst basket on fixed rules instead of per-bar Jev calls | Removes the ~₹110 a day Jev lost; best case about break-even to a few rupees a day | S to run, M to build the decider |
| 2 | Power up the evidence: a 26-week study, a universe-wide news signal table, a report in market-neutral terms | Turns "t 4.09 over 11 days" into a number to believe or drop | S (hours of runtime) |
| 3 | Test the news scoring for look-ahead (hide company and date) | Rules in or out that the one signal is memorised | S |
| 4 | Read filings during the session and study every event, not only the picks | 100× more events; an entry at the moment of the news | M–L |
| 5 | Make costs and fills realistic and require the edge to survive 10 bps a side | Honest paper results; fewer false positives | S |
| 6 | A per-position stop and time exit that the minimum hold cannot block | Cuts the left tail; fixes docs that promise a stop | S–M |
| 7 | Shadow-log every signal and its fixed-horizon outcome in forward paper trading | Leak-free evidence, independent of the capital cap | S |
| 8 | Zero the pre-open gap weight until it is tested; test the gap fade | Removes a prior that the only data points against | S |
| 9 | Test negative-news shorts | A third more events if the −0.47% holds | M |
| 10 | If Jev stays in the price loop: atomic questions, horizon and costs in the question, features gated by information coefficient | Hygiene; do not expect an edge | M |

## What we send Jev, and what comes back

Read from `internal/snapshot/snapshot.go`, `internal/jev/wire.go` and `client.go`, and reproduced by marshalling the golden snapshot (`internal/snapshot/testdata/snapshot_v1.golden.json`) through the real request type (a throwaway test, deleted afterwards). Appendix A holds the full request.

**Every decision is one `POST /v1/systemone` with three parts.**

- **`state`**, 16 numbers and 3 labels, all scale-free: minutes since the open; returns over 1, 5, 15 and 30 one-minute bars; return since the session's first open; the last bar's range; the close's position in the session's low–high range; the last bar's volume against the session's mean bar; the distance from session VWAP; the same returns for NIFTY 50; and the position (side, a size bucket, unrealised percent). No date, no clock time, no symbol (an opaque alias such as `I01`), no absolute price. This is a careful leakage-safe design and it should stay for anything numeric.
- **`questions.action`**, a Choice of BUY / SELL / HOLD: "Given this market state for one instrument, which action should be taken now?", with focus "short-term momentum, volume and VWAP distance against the current position and the index". BUY's example is "positive short-term returns with above-average volume and price above VWAP", SELL's the mirror image.
- **`questions.size`**, a Choice of S / M / L, asked on every call, including those where the action is HOLD.

**What comes back** is per question a choice, a probability for each option and a `confidence`. A real example (the first BUY at 0.90 or more on 2026-09-17, after minute 20, not picked for its outcome):

```text
state   minute 20, FLAT; ret 1/5/15 bars +0.22 / +0.46 / +0.57 %; session +1.46 %;
        range position 1.00; volume 1.43x; VWAP +0.67 %; NIFTY 5-bar +0.09 %
answer  action BUY  P(BUY) 0.94  P(HOLD) 0.04  P(SELL) 0.02  confidence 0.91
        size   L    P(L) 0.72 (confidence 0.58)
tokens  1,046 in, 74 out, ~110 ms
next    the stock moved -0.04 % over 5 minutes, -0.02 % over 15, -0.10 % over 30
```

A request is about 2,200 bytes and 1,000–1,050 input tokens, of which the two questions are about two thirds. A day of 3,000 decisions costs about $0.13 at list price, so cost is not the constraint.

**What the code then does with it.** Of the roughly 7,800 BUY and SELL answers in the 13-day baseline study, the risk gate acted on about 870 (11%). The rest were refused: 3,084 below the probability threshold, 1,781 SELLs with no long to sell (`short_not_allowed`), 685 in the first five minutes, 656 with an order in flight, 456 with the budget full, 235 over the order-rate limit (counts re-derived from the log). 30% of non-HOLD answers came in the first 15 minutes of the session, when the market is at its noisiest.

### Judged against TypeSafe's guidance

Read on 2026-10-04: [how to build with System One](https://docs.typesafe.ai/concepts/how-to-build-with-system-one), [state](https://docs.typesafe.ai/concepts/state.md), [confidence](https://docs.typesafe.ai/confidence.md), [System One](https://docs.typesafe.ai/concepts/system-one.md), [model jaggedness for jev-1.13](https://docs.typesafe.ai/model-jaggedness/jev-1.13.md), [models](https://docs.typesafe.ai/models.md), [API](https://docs.typesafe.ai/api.md).

| TypeSafe says | What we do | Verdict |
| --- | --- | --- |
| "Include only the context relevant to the current questions." | The state carries `schema_version`, the alias, a size bucket and an index block that, for a mid or small cap, is the wrong index (NIFTY 50). | Minor. |
| The code provides the state; the model should not infer or search for context. | Jev must infer relative strength from the stock's returns and the index's returns, and whether a move is large from raw percentages with no volatility scale. | Misses. Compute relative strength and volatility-scaled moves in code. |
| Atomic questions: "Broad questions hide several judgments behind one answer." | One `action` question hides direction, whether anything is held to exit, and urgency, over no stated horizon. | **Misses**, and this is the foundation of the guide. |
| Criteria with `what`, `not_for`, `examples`. | The structure is followed. The content is a trading rule written into the criteria (momentum plus VWAP) and tells Jev SELL means "open a short position" while shorts are off. | Form met, content wrong. |
| Combine independent answer signals with deterministic rules or weighted sums; code owns the decision logic. | The gate maps one answer's probability to an order. | Partly: the gate is deterministic, but it is fed a single signal. |
| Test confidence thresholds against accuracy on real data before deploying them. | Thresholds 0.60 and 0.70 were set first and studied after; the study found no relation between probability and outcome, and they are still in the gate. | **Misses.** |
| Calibration is "measured across groups of predictions" and trained against outcomes. | The probabilities are used as market win probabilities, including in the gate's cost-edge test. | Misreads what is calibrated. |
| The jaggedness page: "Jev is not a calculator"; it does better with semantic representations than with raw numbers; "accuracy falls as the state grows with content unrelated to the decision". | The state is only raw numbers. | A task Jev is documented to be weak at. |
| System One is for "fast, structured decisions" over text: routing, scoring, classification. | News scoring (three typed questions about a headline) is exactly that, and is the one part with signal. | The screen fits; the decision loop does not. |

### Is context missing?

Yes, and here is what, ranked by how much it could matter:

1. **The news itself at decision time.** The watchlist file holds each pick's headlines and Jev's scores; none reach the snapshot, and no filing published after 09:08 is read. The decision loop cannot tell a stock that is up because of results from one that is up because of noise.
2. **The previous close.** `session_return_pct` is measured from today's open, so the opening gap, the biggest single move for a news name, is invisible.
3. **A volatility scale.** A 0.30% five-bar move is large for one stock and nothing for another. Nothing in the state says which.
4. **The right benchmark.** The index block is NIFTY 50. The picks are NIFTY Midcap 150 and Smallcap 250 stocks; on 2026-09-17 the 50 picks averaged +1.0% over the window (37 rose) against NIFTY 50's +0.5%.
5. **The horizon and the costs.** Jev is asked what to do "now" with no horizon, but the bot holds at least an hour and pays 0.15% a round trip. A question that said "over the next hour, a move under 0.3% is not worth acting on" would be a different question.
6. **What the position needs.** Holding time, distance to a stop, time left in the day.

**But more context is not the fix.** The next section shows that 57% of Jev's score is explained by numbers it already has, and that those numbers hold no tradable information at this horizon. Adding features to a decision that has no edge does not create one; a feature earns a place in the state only if it shows a positive information coefficient out of sample (Ranked item 10). The exception is news (the first item in the list above), because the picker already shows text carries some signal.

## What Jev's answers carry

Data: `~/jevtrader-local/data/study/decisions.db`, the 14 Jev runs of the 2026-09-14 to 2026-10-01 baseline study (1-minute cadence, 09:15–10:15, 50 names, 27,180 decisions, 10 days with at least 8 names), and the cached minute and daily candles. Forward return k is the same stock's close k minutes later; **market-neutral** means minus the cross-section mean at the same minute. Rank correlations (IC) are Spearman; `t` is the mean of the daily ICs over its standard error, 10 days, which flatters (see the caveats). Nothing here called Jev or Kite.

**Jev's score is largely a function of the snapshot.**

| Regression of `P(BUY) − P(SELL)` on | R² |
| --- | --- |
| the 5-bar return alone | 0.24 |
| 5-bar return and VWAP distance | 0.40 |
| 12 of the snapshot's numbers (the stock's own features, the index's 5-bar and session returns, the clock) | 0.57 |

Rank correlation of the score with VWAP distance is 0.65, with the range position 0.62, with the 30-bar return 0.57. Jev behaves as a non-linear momentum-and-VWAP rule, as the criteria text asks. That is not a defect in Jev; it is the question we asked.

**That rule has no information at these horizons.** Rank correlation with the market-neutral forward return:

| Signal | +5 min (t) | +15 min (t) | +30 min (t) |
| --- | --- | --- | --- |
| Jev `P(BUY) − P(SELL)` | −0.047 (−2.6) | −0.040 (−1.8) | −0.038 (−1.5) |
| 1-bar return | −0.054 (−4.7) | −0.028 (−3.3) | −0.025 (−2.7) |
| 5-bar return | −0.036 (−2.3) | −0.031 (−1.8) | −0.028 (−1.1) |
| VWAP distance | −0.030 (−1.3) | −0.044 (−1.2) | −0.045 (−1.1) |
| range position | −0.041 (−2.0) | −0.058 (−1.9) | −0.048 (−1.4) |

Jev's score was negative on 7 of 10 days at 15 minutes. The sign is the same across features and horizons, which is why it is worth saying: momentum in these names over the next 5 to 30 minutes is, if anything, slightly reversed (the 1-minute returns have a lag-1 autocorrelation of −0.03, a bid-ask-bounce signature). **But the sizes are tiny.** The market-neutral spread between the top and bottom decile of any of these signals is between −29 and +10 basis points over 5–30 minutes. The largest is the reversal in `session_return_pct` at 30 minutes (the bottom decile, the stocks furthest below their open, beat the cross-section by +16 basis points and the top decile lagged it by 13). A long-only rule buying that bottom decile would about cover a 15-basis-point round trip before any realistic slippage; every other leg is smaller. Nothing here is tradable in either direction, so flipping Jev's sign would not work either, and the one pattern with a size worth a test is a reversal, not a momentum. Jev's BUYs, the only entries the bot can take, moved the right way 44.9% of the time at 5 minutes (mean −0.012%).

**Jev's confidence is not a probability of being right.**

| `P(BUY)` | Calls | Mean 5-minute market-neutral move after |
| --- | --- | --- |
| below 0.1 | 17,076 | +0.008% |
| 0.5–0.7 | 1,438 | −0.030% |
| 0.7–0.9 | 937 | −0.028% |
| 0.9–1.0 | 396 | +0.007% |

No monotone relation. The study's own calibration table says the same (0.9–1.0 predicted, 46.8% moved that way).

**What a trade has to beat.** Market-neutral mean absolute move of these stocks over a holding period (entry times 09:30–15:10, all 937 stock-days with minute candles), and the hit rate a symmetric strategy needs to break even after 0.146% of costs (`p = 0.5 + cost / (2 × mean move)`; assumes move size does not depend on being right):

| Hold | Mean absolute move | Break-even hit rate |
| --- | --- | --- |
| 5 min | 0.13% | above 100% |
| 15 min | 0.22% | 83% |
| 30 min | 0.31% | 73% |
| 1 h | 0.43% | 67% |
| 2 h | 0.61% | 62% |
| 4 h | 0.88% | 58% |

This is the arithmetic behind #53: holding for hours is the right direction, and no decider with a 45% hit rate survives it. The 0.146% is the study's measured charges (₹852 over ₹1.63 million of turnover, 5.2 basis points a leg) plus 2 basis points of slippage a side.

**The early move and the gap** (937 stock-days, 11 days, picks and the random list together; continuation measured from 09:19 or 09:29 so nothing overlaps):

- The first 15 minutes' return does not predict the next hour (IC −0.015, t −0.4) or the rest of the day (−0.000).
- The opening gap (open against the previous close) predicts a **fade**: IC −0.14 at the next hour (t −1.9, negative on 9 of 11 days), −0.03 to the close (not significant). One bucket carries it (gaps of 1–3%, n 48: −0.32% market-neutral over the next hour). Weak, a hypothesis only.

**The news signal, with an interval.** From the stock-day records in `study-sep14-oct01.json` (picks and random list, 856 stock-days, 13 days):

| News signal | Stock-days | Days with one | Window return, market-neutral (95% CI, resampling days) | Day return, market-neutral |
| --- | --- | --- | --- | --- |
| above 0 | 57 | 13 | **+0.29%** (+0.06 to +0.54) | +0.21% (−0.24 to +0.71) |
| below 0 | 18 | 7 | −0.47% (−0.84 to +0.39) | −0.44% |
| zero | 781 | 13 | −0.01% | −0.01% |

The report's daily IC of +0.16 (t 4.09) is a rank correlation on a column that is zero for 91% of the rows, so it is really the 57 and the 18. The positive-news effect clears zero by a small margin over the first hour only; the negative-news effect, the larger number, has an interval that includes zero. **It is a lead worth testing, not a result.**

**What the sample can and cannot tell.** The standard deviation of one stock's return over the 09:15–10:15 window is 1.6%, to the close 2.3%. To detect a mean excess of 0.3% with 80% power and 5% two-sided you need about 220 independent stock-days over the window and 480 to the close. At 1.9 names a day that is about 115 and 250 trading days.

### Caveats on this analysis

- It reads the same 13 days as every earlier study; 10 days enter the daily ICs, and the stocks of one day share the market's move, so the `t` values are generous. Read them as "no positive information", not "reliably negative".
- It covers the first picker's names (the 2026-09-14 to 2026-10-01 picks), not today's news-led list.
- Forward returns use 1-minute closes, which carry bid-ask bounce; the 5-minute figures are the most affected, the 30-minute ones less so.
- Index membership is today's, so there is survivorship (as the study's limitations already say).

## What the web says

Each source is marked **read** (the page was fetched and read for the claim), **summary** (only a search-result summary was seen) or **unverified** (a blog or broker page that was not checked).

**Costs.**

- Zerodha equity intraday ([zerodha.com/charges](https://zerodha.com/charges), **read**, 2026-10-04): brokerage 0.03% or ₹20 an order, whichever is lower; STT 0.025% on the sell side; NSE transaction charge **0.00307%**; GST 18% on brokerage, SEBI fees and transaction charges; SEBI ₹10 a crore; stamp 0.003% on the buy side. `paper.ZerodhaIntraday()` has 0.00297% for the exchange charge. The difference is 0.01 basis point and does not matter, but it shows the table is not being tracked; its own comment says to check it.
- For a ₹2,500 entry the modelled round trip is about ₹2.65 (10.6 basis points) before slippage.
- Small-cap spreads and the real cost of an order in the first minutes after the open are **not measured anywhere in the repository**; the flat 2 basis points is the largest unchecked assumption in every paper result. Ticks are ₹0.05, which is 0.7 basis points at the median pick price of ₹684 and 7 at ₹70; the quoted spread is at least a tick and often several in thin names (inference, not measured). The gate also cannot know when a stock is at a circuit limit; NSE bands are 2/5/10/20% from the previous close ([a secondary source](https://www.oquilia.com/news/nse-individual-stock-circuit-filter-bands), **unverified** against NSE's circular).

**Base rates.**

- SEBI's study of individual intraday traders in the equity cash segment ([summary of the SEBI study](https://taxguru.in/sebi/sebi-study-finds-7-10-individual-intraday-traders-equity-cash-segment-losses.html), **read**): 71% lost money in FY23 (65% in FY19); loss-makers spent a further 57% of their losses on costs; 80% of traders with more than 500 trades a year lost money. Costs are what the people who lose pay most for.
- Barber, Lee, Liu and Odean on Taiwanese day traders, 1992–2006 ([The cross-section of speculator skill](https://faculty.haas.berkeley.edu/odean/papers/day%20traders/The%20Cross-Section%20of%20Speculator%20Skill.pdf), **summary**): fewer than 1% of day traders are predictably profitable. A bot is not a retail trader, but this is the prior a new intraday system should start from.

**Momentum, reversal and the opening.**

- Index-level intraday momentum is documented: the first half hour predicts the last half hour on the S&P 500 (Gao, Han, Li and Zhou, [JFE 2018](https://www.researchwithrutgers.org/en/publications/market-intraday-momentum/), **summary**), with an Indian counterpart for index futures ([Hedging demand and intraday momentum within the Indian stock market](https://www.researchgate.net/publication/383567351_Hedging_Demand_and_Intraday_Momentum_within_the_Indian_Stock_Market), **summary**). Neither says anything about calling single mid and small caps at five minutes.
- Search summaries of the intraday-return literature (US data; for example [Heston, Korajczyk and Sadka](https://arxiv.org/pdf/1005.3535) and [Baltussen, Da and Soebhag on end-of-day reversal](https://www3.nd.edu/~zda/EOD.pdf), neither read in full, **summary**) describe short-horizon reversal that is stronger in smaller stocks and driven largely by bid-ask bounce and temporary liquidity imbalances. That fits the sign in the tables above; it does not make reversal tradable at 15 basis points of cost.
- Opening-range-breakout studies on the NSE exist ([example](https://papers.ssrn.com/sol3/Delivery.cfm/5198458.pdf?abstractid=5198458&mirid=1), **summary**); the ones I saw tested a single stock or index futures. Gap-fill rates quoted on broker blogs ("70% of Nifty gaps fill the same day") are **unverified** and I did not use them; the one measurement here is the weak fade above.
- Overnight against intraday: blog-level claims that India's return has come mostly overnight with the intraday leg flat to negative ([example](https://inthemoneybyzerodha.substack.com/p/the-overnight-drift-why-markets-move), **unverified**). If true, a long-only intraday strategy has to find stock-specific alpha because the market gives it nothing. The 13-day sample here (mean 09:30–15:10 return −0.23%) cannot confirm it.

**News and event-driven.**

- Lopez-Lira and Tang ([arXiv 2304.07619](https://arxiv.org/abs/2304.07619), **read**): a GPT-4 score of headlines captures the initial reaction, predicts the subsequent drift "especially for small stocks and negative news", and the strategy's returns decline as LLM use spreads. This supports the direction of what the picker does and says the negative-news leg may be the bigger one; it also warns the edge is a decaying one.
- Post-earnings-announcement drift in India is documented at a horizon of weeks, stronger in low-market-cap stocks ([study of NSE firms 2002–2017](https://scirp.org/journal/paperinformation?paperid=88060), **summary**). An intraday-only (MIS) system that squares off at 15:10 cannot hold it.
- I found no peer-reviewed study of the minute-level price reaction to NSE corporate announcements. That is the experiment in Ranked item 4.

**LLM- and classifier-driven trading: the pitfalls.**

- *Look-ahead.* A model that has seen the period can recall outcomes rather than predict them. "Profit Mirage" reports Sharpe decay of 51–62% after the model's cutoff across seven agent frameworks ([paper](https://www.researchgate.net/publication/396373592_Profit_Mirage_Revisiting_Information_Leakage_in_LLM-based_Financial_Agents), **summary**); Look-Ahead-Bench reports severe look-ahead bias in commercial models ([overview](https://paperswithbacktest.com/course/look-ahead-bias-llm-trading), **summary**). TypeSafe publishes no training cutoff for jev-1.13.0 ([models](https://docs.typesafe.ai/models.md), **read**), and the news-scoring request does include the company name and the publication timestamp.
- *Long-run results.* FINSABER, across two decades and over 100 symbols, found LLM strategies too conservative in bull markets and too aggressive in bear markets, with earlier positive results attributed to survivorship and data snooping ([arXiv 2505.07078](https://arxiv.org/abs/2505.07078), **read**).
- *Numbers are not where language models help.* Removing the language model from published LLM time-series forecasters did not hurt, and mostly helped ([Tan et al., NeurIPS 2024](https://proceedings.neurips.cc/paper_files/paper/2024/hash/6ed5bf446f59e2c6646d23058c86424b-Abstract-Conference.html), **summary**). TypeSafe says the same about its own model.

## Repository and design critique

What is good, briefly, because it is why the problems above are findable at all: the snapshot's leakage-safe schema and golden test; the single code path for `run` and `replay`; the deterministic gate that no model output can bypass; an append-only decision log; a study command that caches Jev's answers by snapshot so a rerun is free; and reports that print their own limitations. The 13-day baseline study cost $1.18 in tokens. The critique below is of what the system is asked to do, not of the engineering.

**1. Signal quality.** The decision loop's only inputs are price, volume and VWAP numbers, which have no information at these horizons (above). The one signal with some (news) lives in the pre-market screen. The picker's remaining prior, a 0.25 weight on the pre-open gap, has never been tested (a past day has no pre-open) and the one proxy measurement here points the other way (IC −0.14).

**2. Question design.** See the guidance table. In addition, SELL's criteria say "open a short" while `AllowShort` is false: 23% of Jev's non-HOLD answers (1,781 of 7,767) were SELLs with nothing to sell, and the bot paid tokens for each. The size question is asked on every call and means "how strong is the momentum", for which there is no evidence that stronger momentum earns more; sizing should come from volatility and liquidity, in code.

**3. Cadence and horizon mismatch.** Jev is asked what to do "now", and its answers are about the next minutes (that is the horizon its features describe). Since #53 the gate holds a position for an hour and demands two calls five minutes apart. The model's question and the trade's horizon are now different things: a SELL on a long is refused for 60 minutes and then acted on whenever the next one happens to arrive. The study that checked #53 (`study2-hour.md`) used a one-hour window with a one-hour minimum hold, so no decider exit could occur inside it (Jev: 0 trades, −₹91 mark-to-market); it tests nothing about exits. `study2-fullday.md` has 10 Jev trades.

**4. The risk gate.**

- **There is no stop.** `ARCHITECTURE.md` and `docs/run-and-replay.md` say "stops … are never delayed", but there is no stop-loss in `internal/risk` or `internal/engine`: the only automatic exits are the 15:10 square-off, the daily loss limit and the kill switch. This is a wrong sentence in two docs, or a missing feature.
- **The daily loss limit is ₹5,000 on a ₹25,000 budget (20%).** The book would have to lose a fifth of the whole budget in a day (one name alone would have to go to zero, as the per-name cap is also 20%). In practice it never fires; the picks moved up to −5% in a window (IFCI, PFOCUS), which on a ₹2,500 entry is ₹125.
- **The cost-edge check treats Jev's probability as a win rate.** `(2p − 1) × range` with `p = 0.94` and a 0.5% range gives 0.44%, well above the 0.15% cost, so the entry passes; the calibration table says 0.94 is a coin flip.
- **Rotation, cooldown and confirmations** reduce churn (good, and the reason costs fell), but they act on the same uninformative probability.

**5. Cost and fill model.** Charges follow Zerodha's schedule closely. Slippage is a flat 2 basis points with no study or replay flag to change it. By my reading of `paper.OnBar` (not run), a decision at a bar's close fills at the open of the second bar after it (the next bar starts before decision time plus 1 second, so is skipped), a one-minute delay that is neither the 1-second latency nor the real queue; there are no partial fills and no circuit handling. Small-cap spreads in the first minutes are the likeliest source of paper results flattering the system.

**6. Backtest validity.**

- *Look-ahead in the numeric snapshot:* well handled (no date, symbol or price).
- *Look-ahead in the news scoring:* open. The request names the company and gives the timestamp. Whether jev-1.13.0 saw September 2026 is unknown; the study says "may have seen"; nothing tests it.
- *News coverage:* the study reads NSE filings only (RSS has no history); live reads five more feeds. The tested signal is thinner than the live one.
- *Survivorship:* today's index constituents and ASM/GSM lists; delisted names absent.
- *Candles, not ticks:* orders fill on 1-minute candles at the next open ±2 basis points.
- *Sample:* 13 days, then 9 days with any pick. The effective sample is days, not stock-days.
- *The day-close hit rate moved from 54.9% to 42.2%* between the first and the news-led study, on a different list. That is regime or noise, not skill; it was the basis for "trade for hours" (#53). The case for hours holds on the cost arithmetic above, not on that number.
- *Raw, not market-neutral:* hit rates and returns include the day's market drift, so a good day for the market looks like skill (and on 2026-09-17 and 2026-09-18, 33–37 of 50 names rose).
- *Overlapping observations:* decisions every minute on the same stock are not independent; the calibration and hit-rate counts (7,824 scored calls) overstate the evidence.

**7. Evaluation.** Net P&L over a handful of days at ₹25,000 is dominated by noise (the baseline's day-to-day standard deviation for Jev is ₹86 against a mean of −₹110). The report should lead with per-trade or per-event market-neutral return net of costs with a day-clustered interval, and keep P&L as a sanity line.

## Ranked improvements

Ranked by (chance of a real result × size of the result) against effort, with the cheapest decisive test first. Effort: **S** under a day, **M** one to three days, **L** longer. Study runs use `--data-dir data` as in [docs/picker-study.md](../docs/picker-study.md). A flag marked **NEW** does not exist today and is part of the change; everything else is an existing flag. **Primary measure** throughout: mean market-neutral return per name (or per event) from entry to exit, net of costs, with a 95% interval resampling days; the study's `picks buy-and-hold − random buy-and-hold` rows are its closest existing output.

### 1. Trade the catalyst basket on fixed rules; take Jev out of the per-bar loop

**Why.** Per-bar Jev lost in the 13-day baseline (1 positive day in 13; 391 trades; ₹852 of costs; gross −₹583) and in the 9-day news-led study (−₹162, 2 of 9 days positive, 10 trades). The news signal (+0.29% over an hour, market-neutral) is an entry decision known at 08:30; no per-bar call is needed to act on it.

**Change.** Entry: buy each catalyst name at the first 5-minute bar's close (09:20), equal size, no confirmation logic. Exit: a fixed time (for example 11:20, or 15:10) or the stop of item 6, whichever comes first. A small new decider (`basket`) does this inside the existing engine, so the gate, budget and costs apply unchanged. Jev's role is the picker's text judgement only.

**Expected effect.** Removes the Jev-driven loss (about −₹110 a day in the baseline, 59% of it costs). If the news effect is real the net is about +0.14% a name (+0.29% less 0.15% costs): ₹7–13 a day at the current size, which is not income but is a result. If it is not real the net is about −0.15% a name, a known small loss that stops at one round trip a name instead of 30 trades a day.

**Effort.** S to run (the buy-and-hold rows already are this strategy), M to add the `basket` decider.

**Run to confirm or reject** (all existing flags; a 26-week range is the point, see item 2):

```sh
jevtrader study --from 2026-04-01 --to 2026-10-01 --start 09:20 --end 11:20 \
  --deciders hold --random-deciders "" --out reports/basket-0920-1120.md
# then the same with --end 15:10 and with --start 09:30 / 10:00 to see where the effect lives
```

`--start 09:20` makes the window's first open the 09:20 bar's open, so the buy-and-hold rows are the basket entered after the first five minutes; read `picks buy-and-hold − random buy-and-hold`.

**Reject if** the mean market-neutral net return per name is not above +0.10% with the lower bound of its day-resampled 95% interval above zero, or it is positive in fewer than 60% of weeks, or it does not beat the random basket.

### 2. Power up the evidence: 26 weeks, the whole universe, a report in market-neutral terms

**Why.** Every conclusion so far rests on 11 to 13 days and 23 to 57 stock-days. The news effect needs about 220 stock-days to resolve a 0.3% effect; the current picker produces 1.9 a day.

**Change.** (a) Run the existing study over about six months; Kite's 1-minute history comes in 60-day requests and goes back years ([Kite Connect docs](https://kite.trade/docs/connect/v3/historical/), forum-level **summary**; run a three-day dry run first to confirm depth). (b) Run it once with the picker widened to the whole screened pool so the news signal is measured on about 390 names a day, not on 0–5. (c) Change the report: market-neutral returns beside raw, hit rates only at the traded horizon, a gross-versus-costs line for every strategy, and no `t` that treats stock-days as independent.

**Expected effect.** Turns the picker's evidence from a lead into a number: the news column either holds at 100+ days or goes. Costs almost nothing in tokens (the news scoring was $0.0175 for the 13 days; the whole baseline study was $1.18).

**Effort.** S for (a) and (b) (hours to a day of Kite requests at 3 a second; the study caches); S–M for (c).

**Run:**

```sh
# (b) every screened name, no catalyst requirement, negative scores kept (min-score is not range-checked; confirm on a 3-day run)
jevtrader study --from 2026-04-01 --to 2026-10-01 --start 09:20 --end 10:20 \
  --top 400 --max-per-sector 0 --min-score -1 --require-catalyst=false --no-random \
  --deciders hold --out reports/news-universe.md
```

Read the signal table (`news` IC and the mean return above, below and at zero) over about 120 days. Expect hours of Kite requests for 400 names a day at 3 a second (the doc's pace is about 35 seconds a day for roughly 100 requests, so about 2.5 minutes a day here); start with a 12-week range.

**Reject (the news edge) if** the positive-news market-neutral mean over the first hour is under +0.15% (it would not pay 0.15% of costs), or its interval includes zero.

### 3. Test the news scoring for look-ahead

**Why.** The scoring state names the company and the publication time, jev-1.13.0 has no published cutoff, and the whole case for a news-led system rests on scoring historical headlines. Published work on LLM trading finds large look-ahead effects.

**Change.** Add `--anonymize-news` to `screen` and `study`: the state loses `company` and the date, keeps sector and headline text with the company name replaced by "the company" (and a different cache key). **NEW flag.**

**Expected effect.** If the signal survives anonymisation the look-ahead worry is much smaller; if it halves, the historical IC is partly recall and only forward results count. Costs about $0.2 in tokens for a six-month run (the 13-day screens cost $0.0175).

**Effort.** S.

**Run:** the item 2(b) command twice, with and without `--anonymize-news`, same range; compare the `news` rows of the signal table.

**Reject (use of the historical news edge) if** the anonymised positive-news mean falls below half of the named one, or loses its interval. Treat the plain result as unproven until forward paper results agree (item 7).

### 4. Read filings during the session; measure every event

**Why.** Filings arrive all day; the bot reads them only before 09:08. The best-supported use of a language model in the literature is the reaction to news and the drift after it in small stocks, which starts at the filing's timestamp, not at the open. And a six-month event study over all names has hundreds of events a day to test with, where the picks give about two.

**Change.** (a) In the study, score every NSE filing in the universe (NSE's records carry the timestamp, `an_dt`) and measure the market-neutral return from `t + latency` to `t + 15 / 30 / 60 minutes` and to the close, by Jev score bucket, direction and category. (b) If positive, a `run` loop that polls the announcements endpoint every minute, scores new items on watchlist names and creates catalyst entries with the item 1 rules, within the budget.

**Expected effect.** Many more events; an entry at the moment of the news instead of 09:20 or later; direct measurement of the horizon where the effect lives (the first hour on the evidence above). It is the only item here that could make the system worth more than pocket change, and it may find nothing: the reaction can be over in minutes and prices in small caps move on the announcement's first prints.

**Effort.** M for the event study (a), M–L for the live loop (b).

**Run:** `jevtrader study --events` over the item 2 range (**NEW mode**, built on the existing news cache and minute candles); the output is the table of mean market-neutral return by horizon and score bucket, net of the 0.15% cost. There is no existing flag for event-time returns.

**Reject if** no score bucket has a market-neutral mean beyond +0.25% at any horizon from 15 minutes to the close with an interval above zero, or if the effect is entirely in the minutes before a realistic fill (test at `--latency 1s`, `30s` and `120s`, which exists).

### 5. Make costs and fills realistic and require the edge to survive them

**Why.** Every paper result depends on 2 basis points of slippage that nothing checks, and an exchange charge that is out of date. A strategy whose edge is 14 basis points net does not survive 10 basis points of slippage a side.

**Change.** Add `--slippage-bps` to `run`, `replay` and `study`; model slippage as at least one tick plus a liquidity term; record the quote's depth from Kite during forward paper trading to measure real spreads in the first and last half hours; update the exchange charge to 0.00307%.

**Expected effect.** Lowers every paper number and removes false positives. Informs the choice of entry time (the first minutes are the thinnest).

**Effort.** S.

**Run:** any positive result from items 1 to 4, repeated at `--slippage-bps 2, 5, 10, 20` (**NEW flag**) and at `--latency 1s, 30s, 120s` (exists).

**Reject (that result) if** the market-neutral net mean is below zero at 10 basis points a side.

### 6. A per-position stop and time exit that the minimum hold cannot block

**Why.** No stop exists; the docs say one does; the minimum hold blocks the decider's exit for an hour; the loss limit is unreachable. A catalyst name that drops 4–5% in the first minutes stays held to 10:30 or beyond.

**Change.** Add a protective exit in the risk gate: sell when a position is down by the larger of a fixed percent and a multiple of its recent range, and at a maximum holding time; both exempt from `MinHold`, like the square-off. Correct the two docs at once if the stop is not built. **NEW flags** `--stop-pct`, `--max-hold`.

**Expected effect.** Risk reduction, not alpha: it cuts the left tail (in the first study 6 of the 57 positive-news stock-days lost more than 2% over the window, 3 of them more than 4%; the worst was −5.0%) at the price of some whipsaw. It may lower the mean.

**Effort.** S–M.

**Run:** the item 1 command with `--stop-pct 0, 1, 1.5, 2, 3` (**NEW flag**); compare the mean and the worst day.

**Reject (a given stop level) if** it lowers the mean market-neutral net return by more than the tail loss it avoids, measured as the worst-day and fifth-percentile name return.

### 7. Shadow-log every signal and its outcome during forward paper trading

**Why.** The capital cap decides which signals the bot can act on; the evidence wants all of them. Forward results are also the only test free of look-ahead. P&L at ₹25,000 cannot resolve an edge of this size in under a year; per-event returns can resolve it in weeks.

**Change.** On every trading day, for every screened name and every scored filing, store the signal values and the market-neutral return at fixed horizons (5, 15, 30, 60, 120 minutes, close), whether or not the bot traded. Report weekly from the stored rows. The VM already stores the watchlist, the decision log and ticks, so this is a job and a table.

**Expected effect.** A leak-free, growing sample. How fast it resolves depends on how many catalyst names a day the pool really yields, which is unmeasured: 6.7% of the first study's rows had positive news, which on a 390-name pool would be about 25 a day and the 220 stock-days in two weeks; a tenth of that would take five months.

**Effort.** S.

**Run:** the item 2(b) command with `--from <forward start> --to <yesterday>`, weekly, over the forward days only; compare to the historical result and to item 3's anonymised run.

**Reject (the historical news result) if** the forward positive-news mean over 8 or more weeks is under half of the historical one.

### 8. Zero the pre-open gap weight until it is tested; test the gap fade

**Why.** `DefaultWeights` gives the pre-open gap 0.25, a positive prior with no evidence, ranking gap-up names higher. The only measurement available (open against previous close, then the next hour) points the other way: IC −0.14, t −1.9, 9 of 11 days negative. Buying at the open a stock that has already gapped up buys the gap.

**Change.** (a) Set `Gap: 0` in `DefaultWeights` and `--w-gap` until a study supports it. (b) Let the historic phase derive a gap from the first bar's open and the previous close (as the proxy above), so `--w-gap` can be studied. **NEW** (historic-phase gap). (c) Test a fade rule: no entry on a catalyst name that gapped up by more than its typical daily range (the screen's `range` statistic).

**Expected effect.** Small but free; avoids a likely-negative tilt. The fade itself is a hypothesis.

**Effort.** S.

**Run:** after (b), the item 2 command with `--w-gap 0` against `--w-gap 0.25` and `--w-gap -0.25`; compare mean market-neutral returns of the picks.

**Reject (a gap weight) if** the weighted run does not beat `--w-gap 0` by more than its day-clustered interval.

### 9. Test negative-news shorts

**Why.** In the 13-day data negative-news names fell −0.47% market-neutral over the window (n 18, 7 days, interval includes zero), and the literature says drift after negative news is the larger one in small stocks. Intraday short-selling in the cash segment is permitted for most stocks (general knowledge; **unverified** here, and borrow and broker restrictions per stock need checking), and the gate has `AllowShort`, off. A quarter or more of the news events (18 negative against 57 positive in the study) is currently discarded.

**Change.** Study first (the event study of item 4 and the universe study of item 2 already include negative scores), then, if it holds, `--direction both` in the picker with `AllowShort` on for catalyst-short names only. **NEW flags** in `study` (`--direction`, `--allow-short`).

**Expected effect.** A third more events at the same cost per trade if the negative-to-positive ratio holds; intraday shorts are squared off at 15:10 like longs.

**Effort.** M (flags, the SELL-first order path in the paper broker, tests).

**Run:** item 2(b)'s run for the negative column; then the item 1 command with `--direction both --allow-short` (**NEW**).

**Reject if** the negative-news market-neutral mean over the first hour is not below −0.15% with an interval below zero.

### 10. If Jev stays in the price loop: rebuild the question and the state, and gate features by evidence

**Why.** The decision question breaks TypeSafe's main rule, the state is numbers only, and the criteria encode a rule that the data does not support. This does not create an edge, so it is last; do it if per-bar Jev is kept for any role.

**Change.** (a) Replace `action` with atomic Noul/Score questions, each on one property ("is the stock trading above its VWAP with rising volume", "is a long position held that this move threatens"), combined by code; drop `size` and size by volatility in code; remove SELL-to-short while shorts are off. (b) Put the horizon and the cost in the question text ("over the next hour; a move under 0.3% does not pay costs"). (c) Add features as words and ratios Jev can use: gap against the previous close, relative strength against the right index, the move in units of the stock's own recent range, minutes to the close, holding time; describe levels semantically ("near the day's high") rather than as bare numbers. (d) **Admission rule:** a feature or a question enters only if its market-neutral information coefficient over at least 100 days is positive with an interval above zero, measured offline on candles with no Jev call.

**Expected effect.** Fewer, cheaper calls and a cleaner decision log; an edge only if (d) finds a feature that has one.

**Effort.** M (a schema version 2 with its golden file, the questions, and a snapshot selector).

**Run:** `jevtrader study --snapshot v2 --decide-every 15m --deciders jev,momentum,hold` on the item 2 range (**NEW flag** `--snapshot`), comparing against `--snapshot v1` on the identical days and picks; Jev's answers are cached by snapshot, so only v2's cost tokens (about $6 for six months over 50 names a day at 15-minute cadence, at the baseline study's $0.000044 a call).

**Reject if** v2's market-neutral net return per trade is not above v1's by more than its interval, or is not positive.

### Order of work

Items 2, 3 and 7 first (little or nothing to build, and the answers shape everything else); then 1 and 5 on whatever 2 and 3 support; 6 alongside 1 (a stop is needed whatever else is decided); 4 only if 2 and 3 show a news effect worth chasing; 8, 9, 10 last. If item 2 or 3 kills the news signal, the honest outcome is that this system as a whole has no edge to find and the project's value is the platform, not the strategy.

## What is unlikely ever to have an edge

Being blunt, in order of confidence:

1. **Per-minute and per-5-minute direction calls on mid and small caps from price, volume and VWAP numbers, from any model.** The 27,000 decisions say zero or slightly negative information, and costs are about as large as the biggest pattern in them and ten times the typical one. A general text model through an API is not the tool for this even in principle: its documentation says it is weak at numbers, and its probabilities are not about markets.
2. **Using Jev's probability or confidence as a win probability**, anywhere (the entry threshold, the cost-edge check, rotation ranking). Nothing in the log supports it.
3. **More tuning of holds, cooldowns, confirmations and thresholds.** #48 and #53 cut churn and were right to; they cannot add gross edge. The always-HOLD baseline (zero) has beaten Jev in every study.
4. **Sector strength and the recent range** as signals; the study already zeroed them.
5. **More stocks and more trades** to find more edge. The costs scale with trades; the edge does not.
6. **Pure opening-range or momentum rules on single small caps below an hour.** The early-move table above shows nothing, and the opening-range work I found is about an index or a single stock.
7. **Reading the P&L of a ₹25,000 paper account as evidence.** Day-to-day noise is ₹55 to ₹90; any plausible edge is ₹10 a day.

The things that might: a news edge that survives look-ahead and cost tests (items 2, 3, 5), an intraday event reaction that can be entered in time (item 4), and the negative-news leg (item 9). All three are small and decaying, as the literature says.

## Not verified

- Whether jev-1.13.0 saw September 2026 (no published cutoff).
- That the news edge exists beyond 57 stock-days; the whole of item 2 is the check.
- Kite's 1-minute history depth for six months back, and the NSE announcements API's history; a three-day dry run should settle both.
- Real small-cap spreads and slippage in the first and last half hours (nothing measures them).
- Whether `--min-score -1` works as item 2 assumes (`screen.Config.Validate` does not check it, so it should).
- Sources marked **summary** or **unverified** above were not read in full. The cost schedule and the TypeSafe pages were fetched, but through a summarising fetch tool, so only the short phrases in quotation marks are verbatim; read the pages before relying on a paraphrase.
- The analysis reads the first picker's 13 days; it was not re-run on a new list, and it has the usual few-days, correlated-stocks limits.

## Appendix A: a real request, as marshalled

Built by marshalling the golden snapshot through `internal/jev`'s `request` type; the same bytes go to `POST https://api.typesafe.ai/v1/systemone` for every decision (only `state` changes). 2,185 bytes compact.

<details>
<summary>The request body</summary>

```json
{
  "model": "jev-1.13.0",
  "state": {
    "schema_version": 1,
    "instrument": "I01",
    "session": { "minutes_since_open": 40 },
    "features": {
      "returns_pct": { "bars_1": 0.1286, "bars_5": 0.1484, "bars_15": 0.4467, "bars_30": 0.8973 },
      "session_return_pct": 1.2,
      "bar_range_pct": 0.2273,
      "session_range_pos": 0.966,
      "volume_rel": 1.2727,
      "vwap_dist_pct": 0.6847
    },
    "index": {
      "available": true,
      "returns_pct": { "bars_1": 0.1286, "bars_5": 0.1484, "bars_15": 0.4467, "bars_30": 0.8973 },
      "session_return_pct": 1.2
    },
    "position": { "side": "LONG", "size_bucket": "S", "unrealized_pnl_pct": 1.7085 }
  },
  "questions": {
    "action": {
      "type": "choice",
      "instructions": {
        "question": "Given this market state for one instrument, which action should be taken now?",
        "focus": "short-term momentum, volume and VWAP distance against the current position and the index",
        "inspect": "features, position and index in the state"
      },
      "criteria": {
        "BUY":  { "what": "Open or add to a long position now.",
                  "not_for": "Unclear or conflicting signals, or when a long position is already held and the signals have not strengthened.",
                  "examples": ["positive short-term returns with above-average volume and price above VWAP"] },
        "HOLD": { "what": "Take no action now.",
                  "not_for": "A clear, strong signal in one direction.",
                  "examples": ["flat returns, thin volume, or mixed signals across timeframes"] },
        "SELL": { "what": "Reduce or close a long position, or open a short position, now.",
                  "not_for": "Unclear or conflicting signals, or a brief pullback inside an otherwise rising move.",
                  "examples": ["negative short-term returns with above-average volume and price below VWAP"] }
      }
    },
    "size": {
      "type": "choice",
      "instructions": {
        "question": "If the action is taken, which position size bucket fits it?",
        "focus": "how strong and how consistent the signals are",
        "inspect": "features in the state"
      },
      "criteria": {
        "S": { "what": "A small position.", "not_for": "Strong, consistent signals across timeframes.", "examples": ["a weak or mixed signal"] },
        "M": { "what": "A medium position.", "not_for": "Weak signals, or exceptionally strong ones.", "examples": ["a clear signal on one timeframe"] },
        "L": { "what": "A large position.", "not_for": "Weak, mixed or low-volume signals.", "examples": ["a strong signal that agrees across timeframes with high volume"] }
      }
    }
  }
}
```

(The fixture's index block repeats the stock's numbers; in a real request it is NIFTY 50's.)

</details>

## Appendix B: how the numbers were computed

- **Jev rows:** `decisions.db` table `cycles`, `source = 'jev'`, the 14 runs `study-<date>-picks-…-jev` of the baseline study, de-duplicated on (day, token, bar close): 27,180 rows, the same as the study's 26,547 paid plus 633 stored calls. Features come from the `snapshot` JSON, probabilities from the `answer` JSON.
- **Forward return** at k minutes: the same token's price at bar close plus k minutes, from the same run's 1-minute cadence; **market-neutral**: minus the mean of that column over all names at the same bar close.
- **IC:** Spearman rank correlation per day with at least 8 names and 200 rows, averaged over days; `t` = mean / (sd / √days). Pooled (not per-day) Spearman values were also computed and agree in sign (−0.055 at 5 minutes for Jev's score).
- **Decile spreads:** rows from minute 5 on; pooled deciles of the signal (not per minute).
- **Break-even table:** 937 stock-days with 1-minute candles from 09:15 to 15:29 (`study/bars_backfill`), entries every 15 minutes from 09:30, holds of 5 to 240 minutes, market-neutral by stock-day cross-section, cost 0.146% (study charges 852 / 1,634,377 turnover per leg × 2, plus 2 × 2 basis points).
- **Gap and early move:** open of the 09:15 bar against the previous daily close (`screen/candles-*.json`); continuation from the 09:19 close (first 5 minutes) or the 09:29 close (first 15) to the 10:14 close and the 15:29 close, market-neutral by day.
- **News intervals:** the per-stock records (`signals.news`, `return_pct`, `day_return_pct`) in `study-sep14-oct01.json`; mean of the market-neutral return among stock-days with news above (below) zero, with 5,000 resamples of whole days.
- **Power:** n = (1.96 + 0.84)² σ² / δ², σ = 1.60% (window) and 2.34% (day), the sample standard deviations of those returns.
- The scripts are not committed (this report changes no code); the definitions above are enough to redo them from the two local files.
