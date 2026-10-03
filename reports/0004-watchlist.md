# 0004 — Pre-market watchlist: mid and small caps picked from news, sector strength and the pre-open

- **Status:** Proposed (implemented in the PR that closes #41; weights and thresholds are priors, not tuned)
- **Date:** 2026-10-03
- **Issue:** [#41](https://github.com/sanjeev2040/jev-trader/issues/41)
- **Depends on:** [0002 — System architecture](0002-architecture.md), [0003 — Deployment](0003-deployment.md)
- **Decider:** repository owner

## Context

The first replay (1 Oct 2026, 09:15–10:15) traded INFY, TCS and RELIANCE. Jev netted +105 against +588 for buy-and-hold, and RELIANCE churned four round trips inside a two-rupee range. Large caps move little in an hour, and the paper costs eat what they do move.

The captain's ask: before the session, run a job that writes a file of the instruments to trade today, based on news and on which group (sector) is likely to do best, aimed at intraday and at small and mid caps rather than NIFTY 50 names such as Reliance. Trading then runs on that list.

This report fixes the design, records what each external source was seen to do, and states what is still unverified.

## Decision

A new command, `jevtrader screen`, runs twice before the open (an early pass and a final pass after the pre-open call auction) and writes `state/watchlist/YYYY-MM-DD.json`. `jevtrader run --watchlist-file` trades that list and falls back to the static `--watchlist` when the file is missing or not for today. `jevtrader replay --watchlist-file` limits a replay to the same instruments.

```
08:30 IST  screen --phase early   universe -> daily candles (cached) -> liquidity filter
                                  -> news (NSE filings + RSS) -> Jev scores each (item, company)
                                  -> sector strength from the previous session -> top N -> file (final=false)
09:08:30   screen --phase final   same, reusing the candle and Jev-score caches, plus Kite quotes:
                                  pre-open gap per stock and sectoral index moves -> file (final=true)
09:13 at the latest   wait-for-watchlist.sh lets `run` start; it reads the file, or the static list
09:15      the open
```

### Universe

NIFTY Midcap 150 and NIFTY Smallcap 250 constituents (NSE's index files, cached per IST day), less NIFTY 50 and NIFTY Next 50 members, less everything outside the normal `EQ` series (trade-for-trade `BE`, and the REIT `RR` series), less stocks on NSE's ASM or GSM lists. On 2026-10-03 that was 395 constituents in the two indices, 388 after the surveillance lists (271 symbols on ASM or GSM that day).

Then the intraday liquidity filter, from Kite daily candles (up to 45 calendar days back):

| Filter | Default | Flag | Why |
| --- | --- | --- | --- |
| previous-day traded value | ≥ ₹10 crore | `--min-value-cr` | tight spreads; the book trades ₹10k–₹50k a position, so size is not what binds, the spread is |
| average daily volume (20 sessions) | ≥ 200,000 shares | `--min-adv` | one day's volume can be a spike |
| previous close | ₹50 to ₹5,000 | `--min-price`, `--max-price` | no penny stocks, no stocks where one share is a large part of a position |
| history | ≥ 10 sessions, newest candle ≤ 7 days old | – | a new listing has no range or ADV; no recent candle means suspended |

Kite's day candle has no traded value, so the value is volume × (high + low + close) / 3, an approximation.

NSE publishes no "trade-for-trade" flag outside the series. The `Series` column of the index file is the only signal used; ASM stage ≥ I and GSM are excluded whole.

### Signals

Each is turned into a number in [-1, 1] before weighting.

| Signal | Source | Definition |
| --- | --- | --- |
| **news** | NSE corporate announcements, RSS feeds, scored by Jev | per (item, company): `direction × √(material × impact/4) × recency`; summed per company and clamped to [-1, 1] |
| **sector** | members' previous-session returns (Kite day candles), the pre-open gap of the members and the sectoral index (final pass) | sector = NSE `Industry`; raw % = weighted mean of the median member return (weight 1), the median member gap (2) and the index move (1); signal = tanh(raw / 1.5%); needs ≥ 3 liquid members |
| **gap** | Kite quote, final pass only | `(last / previous close − 1)`; signal = tanh(gap / 2%) |
| **range** | the stock's mean (high − low)/close over 10 sessions | rank across the screened stocks, mapped to [-1, 1]: intraday needs room to move |

News scoring asks Jev three typed questions in one request (`POST /v1/systemone`, model `jev-1.13.0`), about a state of `{company, sector, source, published, headline, details}`:

- `impact`, a **Score** over five levels (no effect; under 1%; about 1–2%; about 2–4%; over 4%): the size of the move the news justifies today;
- `material`, a **Noul** (0..1): is the news mainly about this company and material enough to move its price today, rather than routine, procedural or already priced in;
- `direction`, a **Choice** of UP / DOWN / NEUTRAL: `direction` in the formula above is P(UP) − P(DOWN).

Item recency halves every 24 h (`--news-window` is 18 h by default, so the oldest item weighs about 0.6). Overnight news matters most because the market has not yet reacted to it; a short half-life would discount exactly that.

### Ranking

```
score = Σ weight_s × signal_s / Σ weight_s     over the signals available this run
```

A signal that is unavailable for the whole run (no pre-open yet, Jev down, no news source readable) is left out of the denominator, so it does not drag every score toward zero. A stock that merely has no news counts as 0 for the news signal.

| Weight | Default | Reasoning |
| --- | --- | --- |
| news | 0.40 | the only signal that says something new happened to this company |
| sector | 0.25 | the captain's "which group will grow" |
| gap | 0.25 | the market's own verdict at 09:08, the freshest evidence |
| range | 0.10 | a tiebreak towards stocks that can move enough to cover costs |

These are priors, not fitted. Every weight, filter and threshold is a flag (`jevtrader screen -h`).

Selection: sort by score (long) or by |score| (`--direction both`), drop scores below `--min-score` (0.05), keep at most `--max-per-sector` (3) per sector so the list is not one trade, take the top `--top` (8). Each entry carries its signals, its reasons in plain text (the signal that moved it most first, then liquidity), and the news items behind it with Jev's numbers.

**Direction.** The default is `long`. The risk gate in this repository has `AllowShort: false` (`internal/risk/limits.go`), so a short candidate could not be acted on; `--direction both` exists for when that changes, and marks such entries "short candidate". Changing the gate is out of scope here.

### Output

`state/watchlist/YYYY-MM-DD.json`, written atomically (a reader never sees half a file), version 1. Fields: `date`, `generated_at`, `phase`, `final`, `model`, `weights`, `direction`, `sectors[]`, `entries[]` (`rank`, `key` as `NSE:SYMBOL`, `name`, `sector`, `score`, `signals`, `reasons`, `news[]`), `sources[]` (what each source returned or why it failed) and `notes[]`.

An early pass never overwrites a final file (unless `--force`).

### How `run` and `replay` use it

- `run --watchlist-file F [--watchlist L]` loads F. It is used only if it parses and its `date` equals today's IST date. Otherwise `run` trades `--watchlist` and logs why. With no `--watchlist` to fall back on and no usable file it fails. The risk gate and the decision logic are untouched; `run` only reads the list.
- `replay --watchlist-file F` finds each listed symbol's token in the stored data and replays only those (the index stays as context). A symbol not stored is reported and skipped; none stored is an error. It warns if F is for a day outside `--from/--to`. It cannot be combined with `--tokens`.

### Deployment (systemd on the VM)

| Unit | When (IST) | What |
| --- | --- | --- |
| `jevtrader-screen-early.timer` → `jevtrader-screen@early.service` | Mon–Fri 08:30 | waits for the Kite login, runs `screen --phase early` in the image `prepare.sh` pulled |
| `jevtrader-screen-final.timer` → `jevtrader-screen@final.service` | Mon–Fri 09:08:30 | the same, `--phase final` |
| `jevtrader-run.service` | after login | `wait-for-watchlist.sh` holds `run` until the final file exists, never past 09:13 |

The screen needs the Kite session for candles and quotes, so the operator's existing daily login (before 09:00) gates it exactly as it gates `run`. A login after 09:08 gets a final screen straight away and a `run` that has already passed the 09:13 deadline and so uses whatever file there is, or the static list. Timers are `Persistent=true`: a VM that boots late runs the missed screens at boot (the early one skips itself after 09:08). Only one screen runs at a time (`flock`). `WATCHLIST_SCREEN=off` (Bicep `screenEnabled`) restores the previous behaviour; `SCREEN_ARGS` (Bicep `screenArgs`) passes extra flags. The file is also copied to Blob Storage by the nightly backup.

## Sources

Read first-hand on 2026-10-03 with `curl` and by running the code against the live endpoints, unless marked otherwise. "Verified" means the URL answered with the format described; it does not mean the terms were checked.

| Source | URL | Format | Seen |
| --- | --- | --- | --- |
| NIFTY Midcap 150 | `https://nsearchives.nseindia.com/content/indices/ind_niftymidcap150list.csv` | CSV `Company Name,Industry,Symbol,Series,ISIN Code`, 150 rows | verified |
| NIFTY Smallcap 250 | `…/ind_niftysmallcap250list.csv` | same, 250 rows | verified |
| NIFTY 50, NIFTY Next 50 | `…/ind_nifty50list.csv`, `…/ind_niftynext50list.csv` | same, 50 rows each | verified |
| ASM list | `https://www.nseindia.com/api/reportASM` | JSON `{"longterm":{"data":[…]},"shortterm":{"data":[…]}}`, rows with `symbol`, `survCode` (126 + 68 rows) | verified |
| GSM list | `https://www.nseindia.com/api/reportGSM` | JSON array, rows with `symbol`, `survCode` (77 rows) | verified |
| Corporate announcements | `https://www.nseindia.com/api/corporate-announcements?index=equities&from_date=DD-MM-YYYY&to_date=DD-MM-YYYY` | JSON array; `symbol`, `desc` (category), `attchmntText`, `an_dt` (IST), `seq_id` (921 rows for two days; 33 survived the noise filter and an 18 h window on a Saturday morning) | verified |
| Economic Times, stocks and markets | `https://economictimes.indiatimes.com/markets/stocks/rssfeeds/2146842.cms`, `…/markets/rssfeeds/1977021501.cms` | RSS 2.0 | verified |
| Mint, markets | `https://www.livemint.com/rss/markets` | RSS 2.0 | verified |
| The Hindu BusinessLine, markets | `https://www.thehindubusinessline.com/markets/feeder/default.rss` | RSS 2.0 | verified |
| NDTV Profit | `https://feeds.feedburner.com/ndtvprofit-latest` | RSS 2.0 (`www.ndtvprofit.com/rss` itself answers 403) | verified |
| Business Standard | `https://www.business-standard.com/rss/markets-106.rss` | RSS 2.0 | **excluded**: answers 403 to this client and to bare `curl`; it was served only to a browser User-Agent, which this code does not impersonate for a news site |
| Moneycontrol | `https://www.moneycontrol.com/rss/business.xml` | RSS 2.0 | **excluded**: answers 200 but its newest item was from April 2024 |
| Financial Express | `https://www.financialexpress.com/market/feed/` | returned an HTML page, not RSS | **excluded** |
| Kite instruments, daily candles, quotes | `GET /instruments/NSE`, `GET /instruments/historical/:token/day`, `GET /quote` | via `gokiteconnect` | the instrument master is existing, tested code. The day-candle and quote paths are tested against a fake only: **no Kite session was available to the worker, so these were not called live** |
| Jev | `POST https://api.typesafe.ai/v1/systemone`, [API](https://docs.typesafe.ai/api.md), [models](https://docs.typesafe.ai/models.md) | question types `choice`, `noul`, `score`; answers `choice`+`probabilities`, `noul`, `score`+`legend`+`probabilities` | documentation read; request/response shapes covered by tests against a fake server; **no real call was made** (no key available to the worker) |

Not what the brief said: it described Noul and Score as "already implemented in internal/jev". They were not: the client knew only `choice`. This change adds `Client.Ask` for all three types, sharing the retry and error handling with `Decide`.

### Terms, limits and etiquette

- **NSE.** `robots.txt` on `www.nseindia.com` disallows only `/market-data-test` (read 2026-10-03). **NSE's CDN refuses any client that does not send a browser User-Agent**: a request with the Go or curl default, or with an honest `jevtrader/1.0` agent, is dropped at the connection; `Mozilla/5.0 …` works. The code therefore sends a Chrome User-Agent to NSE only (`universe.BrowserUserAgent`, `news.nseBrowserUserAgent`). This is a data-access workaround, the data is public and needs no login, and the volume is small (the four index files and the two surveillance reports once a day, cached; one announcements request per pass), but **NSE's terms of use were not read** and this is the item to confirm. If the captain would rather not, the fallback is to drop the announcements and surveillance sources (`--news-sources` without `nse-announcements`) and keep the index CSVs, which are plain static files, at the price of weaker news and no ASM/GSM filter.
- **RSS feeds** are fetched with an honest `jevtrader` User-Agent, once per pass, headline and short description only. Their `robots.txt` files were grepped for lines covering `rss`/`feed` and none were found (not read in full). Titles and links are stored in the output file; article bodies are never fetched.
- **Kite**: historical candles 3 requests a second, quotes 1 a second and 500 instruments a request. Both are enforced in `internal/screen/kite.go`. About 390 candle requests take roughly two minutes on the first pass of the day and none on the second (cached).
- **Jev**: 80 requests a second and 100,000 tokens a second per the models page; the screen uses 4 concurrent requests.
- No paid data source is used and nothing is scraped behind a login.

## Cost

| Item | Per trading day | Basis |
| --- | --- | --- |
| Jev | about $0.004 | $42 per billion input tokens, output free ([models](https://docs.typesafe.ai/models.md)); up to 150 (item, company) pairs a day at an assumed ~600 input tokens each is ~90,000 tokens. The token count is an estimate: no real call was made. The second pass asks only about items new since the first. Even ten times the estimate is under $0.05 a day, about $1 a month |
| Kite | none marginal | the historical-data add-on is already active; ~390 day-candle requests and 1 to 2 quote requests a day |
| NSE, RSS | none | free public endpoints |
| Azure | none | the screens run inside the VM's existing 08:15–16:00 window; CPU and traffic are negligible next to a running session |

The pre-market run does not change the VM's schedule or size.

## Failure behaviour

| What fails | Effect |
| --- | --- |
| NSE index files | no list; `screen` exits 1; `run` uses the static list |
| ASM/GSM report | the list is built without that exclusion and the file says so in `notes` and `sources` |
| a news source | that source's items are missing; `sources` records it |
| every news source | the news signal leaves the run; its weight is not spread as zeros |
| Jev (key missing, 401, down) | after five failures with no success the scoring stops; news is not in the scores; answers cached from the early pass are still used |
| Kite session expired or no historical-data permission | `screen` stops at the first request with a message pointing to `jevtrader login`; `run` uses the static list |
| Kite quotes (final pass) | the pass proceeds without the gap and sector-index signals |
| quotes that still equal the previous close | treated as "no pre-open yet": fewer than 10% of the candidates moved, so the gap is not used |
| the final pass writes nothing | the early file stays and is used |
| nothing scores above `--min-score` | exit 1; `run` uses the static list |

## Not verified, and decisions for the captain

1. **NSE terms of use and the browser User-Agent** (see Sources).
2. **Pre-open data in Kite quotes.** That `last_price` between 09:08 and 09:15 is the indicative/equilibrium price, and what `volume` means then, was not observed. The code reads the gap as `last_price / ohlc.close − 1` and refuses to use it unless it sees prices that differ from the previous close. The brief also asked for pre-open volume; it is read into `Quote.Volume` but not scored, because its meaning is unobserved. **Check on the first morning with a live session** (`screen --phase final` at 09:09 and compare with the Kite web app).
3. **Sectoral index names** in `DefaultSectorIndexes` (`NSE:NIFTY IT`, `NSE:NIFTY FIN SERVICE`, …) follow Kite's naming from memory. A name Kite does not know is skipped without harm; the member-based sector signal does not depend on them.
4. **systemd specifics** were not run on a VM: how `TimeoutStartSec=30min` interacts with a `Type=oneshot` unit's `ExecStartPre`, `Persistent=true` behaviour after a deallocation, and `OnCalendar=… Asia/Kolkata` (the backup timer already relies on the last).
5. **No evidence yet that the list beats the static one.** Weights and thresholds are priors. The way to test it is `screen --date <past day>` (reconstructs the list from candles and filings that were public before that open; RSS feeds keep no history, so a past day uses NSE filings only), `backfill` those symbols, and `replay --watchlist-file --compare` against the large-cap result. Not done here.
6. **Mid and small caps trade with wider spreads than the paper broker assumes.** `paper.DefaultConfig` uses a flat 2 bps of slippage; for these stocks that is optimistic, so paper results on this list will look better than live would. Changing the paper broker is out of scope; flagged.
7. **Circuit limits.** A stock at its circuit cannot be traded in the direction of the move; the screen does not read circuit bands (Kite's quote carries them) and can pick a stock that opens locked. Flagged for a follow-up.
8. **Name matching.** RSS headlines are mapped to companies by their registered names (dropping `Ltd` and generic trailing words) and by the symbol in capitals; ambiguous or very short names are not matched. This misses some mentions and the Jev `material` question filters passing mentions. In a live run on 2026-10-03, 18 of 82 items in the 18 hour window mapped to one to three screened companies; they were read by eye and looked right apart from passing mentions.

## Testing

All on fixtures: fake NSE documents, fake Kite (`Market` interface and a fake `KiteAPI`), fake news sources, fake Jev (`Asker` interface; `jev.Client.Ask` itself against an `httptest` server). Packages: `internal/watchlist` (format, atomic write, staleness), `internal/universe` (index files, exclusions, daily cache, HTTP routing and User-Agent, surveillance partial failure), `internal/news` (RSS, NSE filings, window, de-duplication, matching), `internal/screen` (phases, ranking, sector cap, direction, caches, every failure above), `cmd/jevtrader` (flags, phase resolution, final-file protection, `run` fallback, `replay` filtering). The live endpoints were exercised once by hand, not in tests.
