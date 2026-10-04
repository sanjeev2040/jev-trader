# Architecture

jev-trader is a realtime NSE trading system written in Go. It reads Zerodha Kite market data, asks the Jev model for a decision on each closed bar, and passes every answer through a deterministic risk gate before an order can exist. Today it **paper trades only**: `--mode=live` cannot place orders ([docs/run-and-replay.md](docs/run-and-replay.md)). One binary, `jevtrader`, has the subcommands `login`, `screen`, `run`, `replay`, `backfill`, `shadow-report` and `version`; a second small command, `tradingday`, tells the scheduled workflow whether the NSE is open.

This page is the map. The decisions behind it are in the reports, which it links rather than repeats:

| Report | Topic |
| --- | --- |
| [0001](reports/0001-language.md) | Why Go |
| [0002](reports/0002-architecture.md) | Package boundaries and the shared code path |
| [0003](reports/0003-deployment.md) | The Azure deployment design |
| [0004](reports/0004-watchlist.md) | The pre-market watchlist |

Operating detail lives in [deploy/README.md](deploy/README.md) (Azure setup and the daily runbook), [docs/run-and-replay.md](docs/run-and-replay.md) (`screen`, `run`, `replay`) and [docs/kite-auth.md](docs/kite-auth.md) (the Kite login).

- [System overview](#system-overview)
- [The trading day on Azure](#the-trading-day-on-azure)
- [The decision loop](#the-decision-loop)
- [The pre-market picker](#the-pre-market-picker)
- [Secrets and configuration](#secrets-and-configuration)
- [CI/CD](#cicd)
- [Local replay and comparison](#local-replay-and-comparison)
- [Development workflow](#development-workflow)

## System overview

`run` and `replay` build the **same engine** (`internal/engine`) from different edges: only the market-data source, the clock, the decider and the broker differ. Packages never reach across those edges, which is what lets a replay exercise the real risk gate and order manager.

```mermaid
flowchart LR
    subgraph Edges["Swappable edges"]
        direction TB
        KT["marketdata/kite<br/>Kite ticker"]
        RS["marketdata/replay<br/>stored ticks and bars"]
        JV["jev + decide<br/>Jev client, baselines, recorded"]
        PB["orders/paper<br/>paper broker"]
        KB["orders/kite<br/>live broker, gated off"]
    end

    subgraph Core["Shared core"]
        direction TB
        EN["engine<br/>one event loop"]
        BA["bars<br/>ticks to bars"]
        SN["snapshot<br/>scale-free market state"]
        RK["risk<br/>gate, budget, kill switch"]
        OM["orders<br/>manager"]
        DL["decisionlog<br/>SQLite"]
        ST["store<br/>Parquet ticks and bars"]
    end

    KT --> EN
    RS --> EN
    EN --> BA --> SN --> EN
    EN <--> JV
    EN --> RK --> OM
    OM --> PB
    OM -.-> KB
    EN --> DL
    EN --> ST
```

| Package | Responsibility |
| --- | --- |
| `cmd/jevtrader` | The CLI: flag parsing and wiring of each subcommand. |
| `cmd/tradingday` | Answers "does the NSE trade on this date?" from `deploy/nse-holidays.yaml`; used by the market-hours workflow. |
| `internal/domain` | Shared types: ticks, bars, instruments, decisions, orders. |
| `internal/marketdata` | The `Source` interface. `kite` is the live ticker; `replay` serves stored data. |
| `internal/store` | On-disk Parquet formats for ticks and bars (the contract with Python analysis). |
| `internal/bars` | Builds 1-minute bars from ticks and derived intervals from those. |
| `internal/snapshot` | The versioned JSON state sent to Jev. It carries no date, time of day, ticker or absolute price, so backtests stay leakage-safe. |
| `internal/jev`, `internal/decide` | The Jev API client, and the `Decider` interface with its implementations: Jev, `hold`, `momentum` and `recorded:<run-id>`. |
| `internal/risk` | The deterministic gate: the only package that can mint the `Approval` the order manager requires. It also emits risk-driven exits and owns the kill switch and the capital budget. |
| `internal/orders` | The order manager and the `Broker` interface; `paper` fills against the market-data stream, `kite` is the live broker that refuses while live trading is not approved in code. |
| `internal/engine` | The single event loop that composes all of the above, including own-position rotation. |
| `internal/decisionlog` | Append-only SQLite log of every cycle and order event; also the source for `recorded:` replays. It also holds the shadow signals of every candidate, traded or not, and their outcomes. |
| `internal/shadow` | Fills the shadow signals' outcomes at fixed horizons from the recorded bars and summarises them (`jevtrader shadow-report`). |
| `internal/clock` | Wall clock for `run`, simulated clock for `replay`. |
| `internal/replay` | The replay runner, its report, the instrument-by-instrument walkthrough and the market comparison. |
| `internal/screen`, `internal/universe`, `internal/news`, `internal/watchlist` | The pre-market picker: universe, news sources, signals and ranking, and the watchlist file format. |
| `internal/kiteauth` | The daily Kite login and the saved session. |
| `internal/config` | Secrets (redacted `Secret` type), the Key Vault reader and the capital budget. |
| `internal/nsecal` | The NSE holiday calendar. |

Infrastructure is in `infra/main.bicep`; the scripts and systemd units that run on the VM are in `deploy/vm/`.

## The trading day on Azure

One Ubuntu VM in Central India is started on NSE trading mornings and deallocated by Azure at 16:00 IST ([report 0003](reports/0003-deployment.md)). All times are IST.

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub Actions<br/>market-hours.yml
    participant VM as Azure VM<br/>systemd units
    participant Op as Operator
    participant Kite as Kite Connect
    participant Blob as Blob Storage

    GH->>GH: 08:15 Mon-Fri: tradingday checks the holiday calendar
    GH->>VM: az vm start (OIDC identity "schedule")
    VM->>VM: jevtrader-prepare: read jev-image tag, pull image, load secrets
    Op->>VM: before 09:00: ssh tunnel and jevtrader-login
    VM->>Kite: login, saves the day's session
    VM->>VM: 08:30 screen --phase early (waits for the login)
    VM->>VM: 09:08:30 screen --phase final (adds the pre-open gap)
    VM->>VM: wait-for-watchlist, never past 09:13
    VM->>Kite: jevtrader run --mode=paper, from 09:15
    Note over VM: entries from 09:20 to 15:00,<br/>square-off from 15:10
    VM->>Blob: 15:40 backup of Parquet, decision log and watchlist
    VM->>VM: 16:00 Azure auto-shutdown deallocates the VM
```

The units, in the order they matter:

| Unit | When | What it does |
| --- | --- | --- |
| `market-hours.yml` | 08:15 Mon–Fri (02:45 UTC) | Skips weekends and NSE holidays, then `az vm start`. A manual run with `force` starts the VM on a closed day. |
| `jevtrader-prepare` | every boot | Reads the VM's `jev-image` tag from the instance metadata service, pulls that image from ACR with the managed identity, writes the secrets to a `0600` file on tmpfs. Retries every 30 seconds. |
| `jevtrader-login` | by hand, before 09:00 | Prints the Kite login URL; the SSH tunnel carries Kite's redirect to the listener on the VM. |
| `jevtrader-screen-early.timer` | 08:30 | `screen --phase early`: news and the previous session. Waits for the login. |
| `jevtrader-screen-final.timer` | 09:08:30 | `screen --phase final`: adds the pre-open gap. |
| `jevtrader-run` | after prepare and login | Waits for a valid Kite session, then for the final watchlist but never past 09:13, then `jevtrader run --mode=paper`. Restarts every 30 seconds on failure. |
| `jevtrader-backup.timer` | 15:40 | Parquet files, a consistent SQLite `.backup` of the decision log and the day's watchlist go to Blob Storage. |
| Azure auto-shutdown | 16:00 | Deallocates the VM. Nothing in GitHub stops it. |

If nobody logs in, nothing trades and the VM is still deallocated at 16:00. The risk gate's own limits for the session are in [The decision loop](#the-decision-loop).

## The decision loop

Every closed bar of a watched instrument that falls on the decision cadence (every fifth minute by default, `--decide-every`) starts one decision cycle. A single goroutine owns all mutable trading state; deciders run in their own goroutines and return their answers on a channel.

```mermaid
flowchart TD
    T["Ticks<br/>Kite ticker or stored data"] --> B["bars<br/>1-minute bars"]
    B --> C{"Bar closed, in the<br/>trading window?"}
    C -- no, warm-up --> W["Kept for indicators<br/>and marks, no decision"]
    C -- yes --> S["snapshot<br/>scale-free state, opaque alias"]
    S --> J["Decider: Jev<br/>deadline 800 ms"]
    J -- "failure or timeout" --> H["Substituted HOLD"]
    J -- BUY, SELL or HOLD --> G
    H --> G

    subgraph G["risk gate: first failing check wins"]
        direction TB
        G1["kill switch, session window, daily loss limit"]
        G2["probability threshold, entry confirmation,<br/>minimum hold, cooldown, order in flight, no shorts"]
        G3["position cap, gross cap, capital budget"]
        G4["order rate and daily order budget"]
        G1 --> G2 --> G3 --> G4
    end

    G --> V{"Verdict"}
    V -- rejected or HOLD --> D
    V -- "budget full" --> R["Rotation: sell the weakest own position,<br/>re-ask the gate when it fills"]
    R --> D
    V -- approved --> D["decisionlog<br/>cycle row written first"]
    D --> O["order manager"]
    O --> P["paper broker<br/>fills against the stream"]
    P --> E["order_events row<br/>per broker update"]
```

Points the diagram compresses:

- **Every signal is shadow-logged.** Beside each cycle row `run` writes a `shadow_signals` row (and one per picker entry at the start of the day), whether or not the gate let it trade. `shadow-report` fills what the market did at +15m, +60m, the window end and the day close from the recorded bars. See [docs/run-and-replay.md](docs/run-and-replay.md#the-shadow-log-and-jevtrader-shadow-report).
- **The log comes before the order.** The cycle row (snapshot, answer, verdict, intent) is written before any order is submitted; an `order_events` row follows each broker update.
- **Risk exits do not wait for a decider.** The per-position stop (2% below the average price by default), the optional time exit, the square-off, the daily loss limit and the kill switch produce exits on every tick or bar, logged as cycles of their own with the numbers behind them. The stop and the time exit sell one position; the other three flatten the book.
- **Trade for hours.** The defaults hold a position for at least an hour, wait an hour before re-entering a stock, and enter only on a call of at least 0.70 repeated on two consecutive decisions, five minutes apart. The stop, the time exit, the square-off and the kill switch are never delayed by any of it. See [docs/run-and-replay.md](docs/run-and-replay.md#trading-for-hours).
- **The default session limits** (`risk.DefaultLimits`): no entries before 09:20 or at and after 15:00, and every position flattened from 15:10.
- **Budget and sizing.** The bot trades within a total rupee budget (₹25,000 unless configured) that the gate enforces on every order, sized in buckets S, M and L. See [Secrets and configuration](#secrets-and-configuration) for where the number comes from and [docs/run-and-replay.md](docs/run-and-replay.md#capital-budget) for the rules.
- **Rotation.** When the budget is full, the gate may sell one of the bot's *own* positions to make room. The entry waits up to two minutes for that sale to fill, then goes back to the gate; a stale one is logged as `rotation_abandoned`. Positions the bot did not open are never sold.
- **Kill switch.** A `KILL` file in the state directory, `SIGUSR1`, too many consecutive broker rejections, or a refused order IP trips it.
- **Failures are HOLDs.** Any decider error or timeout becomes a HOLD, counted separately in the run's stats.

## The pre-market picker

`jevtrader screen` builds the day's watchlist in two passes and writes `state/watchlist/<date>.json`. Sources, weights and limits are in [report 0004](reports/0004-watchlist.md) and [docs/run-and-replay.md](docs/run-and-replay.md#jevtrader-screen).

```mermaid
flowchart TD
    U["Universe<br/>NIFTY Midcap 150 + Smallcap 250<br/>less NIFTY 50 and Next 50"] --> X["Drop non-EQ series and<br/>ASM / GSM stocks"]
    X --> L["Liquidity filter from Kite daily candles<br/>traded value, average volume, price band"]
    L --> SIG

    subgraph SIG["Signals"]
        direction TB
        N["News, last 18 hours<br/>NSE announcements + business RSS"]
        N --> NJ["Jev scores each item<br/>impact, materiality, direction"]
        SE["Sector strength<br/>weight 0 by default"]
        GP["Pre-open gap<br/>final pass only"]
        RG["Recent range<br/>weight 0 by default"]
    end

    SIG --> CAT["Keep stocks with a positive news catalyst<br/>(material, positive, large enough, by Jev)"]
    CAT --> RANK["Weighted rank<br/>news 0.6, gap 0.25, sector 0, range 0<br/>unavailable signals are left out"]
    RANK --> TOP["Up to 50, at most 8 per sector<br/>fewer on a quiet day"]
    TOP --> F[("state/watchlist/date.json")]
    F --> RUN["jevtrader run --watchlist-file<br/>today's file only, else static --watchlist"]
```

- **Two passes.** `early` runs at 08:30 on news and the previous session; `final` runs at 09:08:30 and adds the pre-open gap and sectoral indices (`final=true` in the file). An early pass never replaces a final file unless `--force`. Daily candles and Jev's news scores are cached for the day, so the second pass is short.
- **A past `--date`** uses the `historic` phase: no pre-open exists and only NSE filings supply news.
- **The fallback is deliberate.** Whatever the screen does, `run` starts (on a quiet day with an empty list it idles rather than trading the static list). If the file is missing, stale or unreadable, it trades the static `watchlist` from the deployment parameters, and it refuses to start only when it has neither.
- **Needs** a Kite session and `KITE_API_KEY`; without `JEV_API_KEY` the news is fetched but not scored.

## Secrets and configuration

No secret lives in GitHub, in the image or in the repository. Key Vault holds the credentials, and each place that runs the code reads them with its own identity.

```mermaid
flowchart LR
    KV[("Azure Key Vault<br/>kite-api-key, kite-api-secret,<br/>jev-api-key, trading-capital")]

    subgraph Azure["On the VM"]
        MI["VM managed identity<br/>Key Vault Secrets User"] --> PR["prepare.sh"]
        PR --> SE["/run/jevtrader/secrets.env<br/>0600, tmpfs"]
        SE --> DK["docker run<br/>-e passes names, not values"]
    end

    subgraph Local["On a laptop"]
        AZ["az login session"] --> CFG["internal/config<br/>SecretSource"]
        CFG --> LJ["jevtrader screen, run, replay"]
    end

    KV --> MI
    KV --> AZ
    ENV["Environment variables<br/>fallback"] -.-> CFG
    DK --> BIN["jevtrader in the container"]
```

- **On the VM,** `prepare.sh` uses the VM's managed identity to read the secrets and writes them to a `0600` file on tmpfs. systemd loads that as an `EnvironmentFile`, and the scripts pass variables into the container by name.
- **Locally,** setting `JEVTRADER_KEYVAULT` to the vault name makes the commands read the same secrets through the `az` CLI and your own `az login`. The environment is the fallback, and when the variable is unset secrets come from the environment alone. `config.Secret` prints as `[redacted]` in logs, JSON and formatted output.
- **The capital budget** is the Key Vault secret `trading-capital`, read like the others but treated as configuration, not a credential: it is logged. Order of precedence: the `--capital` flag, then Key Vault, then `JEVTRADER_TRADING_CAPITAL`, then the ₹25,000 default. Changing it needs no deploy; it applies at the next start.
- **GitHub holds identifiers only** (tenant, subscription, resource names, client ids). The two workflow identities use OIDC federated credentials with no client secret, and CI has no right to change infrastructure.
- **Kite session.** The access token comes from the daily `login`, not from Key Vault ([docs/kite-auth.md](docs/kite-auth.md)).

## CI/CD

Three workflows in `.github/workflows/`. The deploy action is only a tag: a merge never disturbs a running session.

```mermaid
flowchart TD
    PR["Pull request into dev or main"] --> CI["ci.yml"]

    subgraph CIJ["CI jobs"]
        direction LR
        T1["test<br/>build, vet, test -race"]
        T2["docker<br/>build, not pushed, smoke test"]
        T3["bicep<br/>compile template and params"]
    end

    CI --> CIJ
    CIJ -- green --> DEV["merge to dev"]
    DEV -- "push to dev runs CI again" --> CI
    DEV --> REL["PR dev into main"]
    REL --> MAIN["merge to main"]
    MAIN --> RW["release.yml<br/>environment: production"]
    RW --> ACR[("ACR<br/>jevtrader:12-char-sha")]
    RW --> TAG["VM tag<br/>jev-image = sha"]
    TAG -. "read at the next boot" .-> NEXT["Next morning's VM start<br/>pulls that image"]

    CRON["market-hours.yml<br/>cron 08:15 IST Mon-Fri<br/>runs on the default branch dev"] --> OPEN{"tradingday:<br/>market open?"}
    OPEN -- yes --> START["az vm start"]
    OPEN -- no --> SKIP["VM stays deallocated"]
    START --> NEXT
```

- **CI** runs on every pull request into `dev` and `main` and on pushes to `dev`. It needs no secrets, and superseded runs on the same ref are cancelled.
- **Release** builds the image once, pushes it to ACR as `jevtrader:<12-char-sha>` and sets the VM tag `jev-image=<sha>`; the VM never reads a moving tag. **Rollback** is a manual run of the workflow with `rollback_sha` set to an earlier tag; it skips the build.
- **Market hours** checks the holiday calendar with `cmd/tradingday` (a manual `force` overrides it), then starts the VM. Azure auto-shutdown handles the stop.
- **Both Azure workflows skip with a notice and stay green** until the repository variables from the [setup guide](deploy/README.md) exist.
- **Two OIDC identities, one per environment:** `production` may push to ACR and tag the VM; `market-hours` may only read, start and deallocate it.

## Local replay and comparison

`jevtrader replay` runs the real engine, risk gate and paper broker over stored market data with a simulated clock, and reports each run against baselines. It is how a change is tested without waiting for a trading day.

```mermaid
flowchart LR
    subgraph Data["Stored data"]
        direction TB
        TK["Parquet ticks<br/>recorded by run"]
        BF["Kite historical candles<br/>jevtrader backfill"]
    end

    WL["Watchlist file<br/>from screen"] -.-> RP
    Data --> RP["jevtrader replay<br/>--source ticks, bars or backfill"]
    SEC["Key Vault via az login"] -.-> RP

    RP --> E["engine<br/>simulated clock, paper broker,<br/>session window --start / --end"]
    E --> LG[("decisions.db<br/>run per decider")]
    E --> OUT["Report"]

    subgraph Deciders["Deciders"]
        direction TB
        D1["hold"]
        D2["momentum"]
        D3["jev"]
        D4["recorded:run-id"]
    end
    Deciders --> E
    LG -. "recorded:run-id replays<br/>an earlier run's answers" .-> D4

    OUT --> WT["--walkthrough<br/>session by instrument"]
    OUT --> CMP["--compare<br/>price after each BUY and SELL,<br/>PnL against buy-and-hold,<br/>hold and momentum"]
```

- **Sources.** `ticks` replays recorded ticks, `bars` builds bars from them, and `backfill` uses Kite historical candles that `jevtrader backfill` stored.
- **Deciders.** The default is `hold,momentum`. `jev` calls the live model, so its answers may already know the past; a `recorded:<run-id>` decider re-answers from an earlier run's stored decisions, so everything except the model can be retested. The look-ahead risk is covered in [report 0002](reports/0002-architecture.md).
- **Session window.** `--start` and `--end` limit trading to part of the day. Earlier bars still warm the indicators up, and later data is used only for the comparison.
- **`--compare`** adds the price a few minutes after each BUY and SELL (`--lookahead`, default 5m, 15m, 30m) and the PnL against buy-and-hold and the hold and momentum baselines, which it replays too. **`--walkthrough`** narrates the session instrument by instrument.
- **`--watchlist-file`** replays only the instruments a `screen` run picked.
- **The capital budget** applies as in `run`; `--capital` overrides it for the replay.

## Development workflow

Work moves from an issue to `dev` through a worker and the `no-mistakes` pipeline, then reaches `main` as a reviewed release.

```mermaid
flowchart LR
    I["GitHub issue"] --> WK["Worker agent<br/>branch in its own worktree"]
    WK --> NM

    subgraph NM["no-mistakes pipeline"]
        direction TB
        RV["review, tests, lint, docs"] --> FX["Pipeline applies fixes<br/>decisions go to the supervisor"]
        FX --> PU["push and open PR"]
        PU --> CK["CI on the PR"]
    end

    NM --> PRD["PR into dev<br/>Closes #issue"]
    PRD --> MD["merge to dev"]
    MD --> RL["PR dev into main<br/>release workflow"]
```

- **Branches.** `dev` is the default branch and where work lands; `main` is what gets released. Every change goes through a pull request, and CI is the same `ci.yml` described above.
- **Issue to PR.** A worker builds one issue on its own branch. `no-mistakes` reviews the diff, runs tests, lint and docs checks, applies the fixes itself and opens the PR; findings that need a human decision are escalated rather than answered by the worker.
- **Release.** Merging `dev` into `main` runs [the release workflow](#cicd), which takes effect at the next VM start.
- **Before pushing,** the same checks CI runs can be run locally: `go build ./... && go vet ./... && go test -race ./...`.
