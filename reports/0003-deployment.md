# 0003 — Azure deployment that runs only during NSE market hours

- **Status:** Accepted (option B)
- **Date:** 2026-10-02
- **Issue:** [#23](https://github.com/sanjeev2040/jev-trader/issues/23)
- **Depends on:** [0002 — System architecture](0002-architecture.md) (Accepted)
- **Feeds:** #24 (infrastructure code)
- **Decider:** repository owner

## Context

`jevtrader run` records ticks, builds bars, asks Jev for decisions and paper-trades. Live Kite order placement is disabled for now. Later it needs a whitelisted static outbound IP. The owner wants a cost-effective Azure deployment that consumes resources only while the market is open and sits idle otherwise. GitHub Actions will build and tag an image when `dev` merges to `main`, then deploy it. The repository has no CI yet.

[0002](0002-architecture.md) already assumed "one small Linux VM in an Indian region with a reserved static IP". This report tests that assumption against scale-to-zero options and fixes the schedule, secrets, storage and release flow.

### Facts the decision rests on

Facts about the code were read from this repository at `896c682`. Azure prices and docs were read on 2026-10-02.

1. **The service is one long-lived process.** `run` stays up until its context ends. It has no session-end exit and no NSE holiday calendar. Its only session logic is the risk gate's entry window and its 15:10 IST square-off. Whatever schedules the compute must also decide which days to skip.
2. **Market window.** Equity pre-open starts 09:00 IST and the market closes 15:30. The issue says "about 09:00–15:45". This report plans compute from **08:30 to 16:00 IST** (7.5 h): about 30 minutes for the manual login, then 15 minutes after the close to flush files.
3. **Trading days.** 2026 has 16 weekday exchange holidays ([list](https://zerodha.com/marketintel/holiday-calendar/)). That leaves 245 trading days, about 20.4 a month. Costs use **21 days a month**. Special sessions on weekends (for example Muhurat trading on Sunday 2026-11-08) also exist.
4. **Daily login is manual and bound to the host.** `jevtrader login` starts a listener on `127.0.0.1:8765` and the browser redirect must reach it ([docs/kite-auth.md](../docs/kite-auth.md)). The process that logs in therefore needs a loopback path from the operator's browser. On a VM that is an SSH tunnel. `run` verifies the saved session at startup and exits if there is none. It never logs in by itself.
5. **Kite static IP.** Only order endpoints are checked against the registered IP. WebSocket data, order book, positions and login work from any IP. Cloud IPs are allowed. Kite allows one primary and one optional secondary IP, and frequent changes are not allowed ([forum](https://kite.trade/forum/discussion/15912/preparing-to-comply-with-sebis-retail-algo-rules-static-ip-ratelimits-order-types), [search summary](https://www.quotaguard.com/blog/kite-connect-static-ip)). The change limit is not stated in the official post and was not verified. **The IP must therefore be a resource that survives for the life of the project.**
6. **Storage.** Ticks and bars are Parquet files rotated every 15 minutes. The decision log is SQLite in WAL mode. It must survive a crash, because order recovery reads it on restart ([0002](0002-architecture.md), "Order manager"). SQLite documents that [WAL does not work over a network filesystem](https://www.sqlite.org/wal.html).
7. **Unit tests run on fixtures and need no secrets.** CI can therefore run on plain GitHub-hosted runners with no Azure access.

## Options

All three run the same Linux amd64 container image, in Central India.

| Option | How it saves money | Shape |
| --- | --- | --- |
| **A. Azure Container Apps scheduled Job** | Compute is billed per second only while the job runs. | A workload-profiles environment with a Consumption profile. A `Schedule` job starts at 03:00 UTC (08:30 IST) with a long replica timeout. Cron is evaluated in UTC ([docs](https://learn.microsoft.com/en-us/azure/container-apps/jobs)). |
| **B. B-series VM started and deallocated on a schedule** | A deallocated VM bills no compute. The disk and static IP keep billing. | One Linux VM with a static public IP and a systemd unit. Something starts it each trading morning and an auto-shutdown deallocates it each evening. |
| **C. Azure Container Instances started and stopped on a schedule** | A stopped container group stops its meters ([FAQ](https://learn.microsoft.com/en-us/azure/container-instances/container-instances-faq)). | One container group, started and stopped by `az container start/stop`. |

Options considered and not costed: AKS (a control plane and nodes for one process), Azure Functions or Logic Apps (wrong shape for a 7-hour process), and a 24/7 VM (the baseline in the cost table).

## Cost table

All prices are USD list prices for Central India from the [Azure Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices), read on 2026-10-02. They exclude GST and are not converted to INR.

**Assumptions:**
- 7.5 h × 21 days = **157.5 compute hours a month** (567,000 seconds).
- A 730-hour month for always-on items.
- 25 GiB of backups or storage.
- A 64 GiB disk.
- Container options get 0.5 vCPU and 1 GiB. The VM gets 2 vCPU and 4 GiB, the smallest size with comfortable headroom, because real memory use is not yet measured.

### Unit prices used

| Item | Price | Arithmetic basis |
| --- | --- | --- |
| VM B2als v2 (2 vCPU, 4 GiB), Linux | $0.0246 / h | verified |
| VM B2ats v2 (2 vCPU, 1 GiB), Linux | $0.00615 / h | verified |
| Standard SSD E6 (64 GiB) | $5.28 / month | verified |
| Standard static public IPv4 | $0.005 / h | verified |
| NAT Gateway | $0.045 / h + $0.045 / GB | verified for the **Global** meter. No Central India meter was returned, so the global price is assumed to apply. |
| ACI vCPU / memory | $0.0426 / vCPU-h, $0.00466 / GB-h | verified |
| ACA Consumption vCPU / memory | $0.000024 / vCPU-s, $0.000003 / GiB-s | verified. Free each month per subscription: 180,000 vCPU-s and 360,000 GiB-s ([billing](https://learn.microsoft.com/en-us/azure/container-apps/billing)). |
| ACR Basic | $0.1666 / day | verified. The 10 GiB included storage is from memory, not verified. |
| Azure Files (standard, LRS) | $0.066 / GB-month | verified for data stored. Transaction charges were not priced. |
| Blob Hot LRS | $0.02 / GB-month | verified |
| Key Vault operations | $0.03 / 10,000 | verified |
| Log Analytics ingestion | $0.00 for the first 5 GB, then $3.22 / GB | verified |

### Monthly estimate

| Line | A. ACA Job, today (paper) | A. ACA Job, live (static IP) | B. VM, today and live | C. ACI, today (paper) | C. ACI, live (static IP) |
| --- | ---: | ---: | ---: | ---: | ---: |
| Compute | 0.5 vCPU × 567,000 s = 283,500 vCPU-s, minus 180,000 free, × $0.000024 = $2.48. 1 GiB × 567,000 s = 567,000 GiB-s, minus 360,000 free, × $0.000003 = $0.62. **$3.11** | $3.11 | 157.5 h × $0.0246 = **$3.87** | 157.5 h × (0.5 × $0.0426 + 1 × $0.00466) = **$4.09** | $4.09 |
| Disk or file share | 25 GiB Azure Files × $0.066 = $1.65 | $1.65 | 64 GiB E6 = $5.28 | $1.65 | $1.65 |
| Static IP | – | $0.005 × 730 = $3.65 | $0.005 × 730 = $3.65 | – | $3.65 |
| NAT Gateway | – | $0.045 × 730 = $32.85 | – | – | $32.85 |
| Registry (ACR Basic) | $0.1666 × 365 / 12 = $5.07 | $5.07 | $5.07 | $5.07 | $5.07 |
| Backup of 25 GiB to Blob Hot | – (data already in Files) | – | 25 × $0.02 = $0.50 | – | – |
| Key Vault, logs, alerts, schedule | ≈ $0 | ≈ $0 | ≈ $0 | ≈ $0 | ≈ $0 |
| **Total** | **$9.83** | **$46.33** | **$18.37** | **$10.81** | **$47.31** |

Notes:
- **Key Vault, logs, alerts and schedule are ≈ $0 on the assumption of low use.** Key Vault operations are $0.03 per 10,000. Log ingestion stays under the free 5 GB. A budget alert is free. A metric alert rule and Azure Automation minutes are not priced here (unverified). The recommended scheduler, a GitHub Actions workflow, uses minutes from the repository plan's allowance (also unverified).
- **Always-on comparison.** The same VM running 24/7 costs 730 h × $0.0246 = $17.96 for compute alone. Market-hours scheduling cuts compute by 78%.
- **VM downsizing.** Moving to B2ats v2 (1 GiB) cuts compute to 157.5 × $0.00615 = $0.97 and the total to $15.46. Do this only after measuring resident memory over a recorded session.
- **GHCR instead of ACR** removes the $5.07 registry line. See the registry criterion for why that is not free of cost.
- **A NAT Gateway that exists only in market hours** would cost 157.5 h × $0.045 = $7.09 instead of $32.85. It would need a create, attach, detach and delete cycle every trading day around a container group. That is brittle, and it is not recommended.
- **Data transfer.** Ticks come in free. Outbound traffic is small, to Jev and to Kite. It is not priced here (unverified).

## Criteria matrix

✓ meets it, ~ meets it with work or a caveat, ✗ does not meet it without a code or cost change.

| Criterion | A. ACA Job | B. VM | C. ACI |
| --- | --- | --- | --- |
| **Monthly cost today (paper)** | ✓ $9.83 | ~ $18.37 | ✓ $10.81 |
| **Monthly cost with static IP (live)** | ✗ $46.33 | ✓ $18.37, unchanged | ✗ $47.31 |
| **Static outbound IP now** | Not needed for paper trading. Outbound IPs "might change over time" ([networking](https://learn.microsoft.com/en-us/azure/container-apps/networking)). | ✓ The attached static IP is the outbound IP. | Not needed. The egress IP is not exposed or stable. |
| **Static outbound IP later** | ~ Needs a workload-profiles environment, a VNet with a /27 subnet and a NAT Gateway. Consumption-only environments cannot do it. | ✓ Register the existing IP in the Kite console. No migration. | ~ Needs a VNet, a delegated subnet and a NAT Gateway. "Container group IP addresses are subject to change." |
| **Daily manual Kite login** | ✗ No way to tunnel a browser redirect into the job. Needs a code change so the token comes from a store. | ✓ Already works: SSH tunnel as in [docs/kite-auth.md](../docs/kite-auth.md). Zero code. | ✗ Same as A. `az container exec` is a shell, not a port forward. |
| **Secrets** | ~ Job secrets or Key Vault references. A start permission also exposes job secrets ([docs](https://learn.microsoft.com/en-us/azure/container-apps/jobs)). | ✓ Key Vault read through the VM's managed identity. | ~ Key Vault via managed identity, or secure environment variables. |
| **Persistent ticks and bars** | ~ Azure Files mount. Works for append-only Parquet. | ✓ Local managed disk, persists across deallocation. | ~ Azure Files mount. |
| **SQLite decision log** | ✗ WAL cannot live on an SMB share. Local ephemeral disk loses the log, and order-recovery state, on any replica restart. Platform maintenance can interrupt long jobs. | ✓ Local disk. WAL works. A crash or reboot keeps the file. | ✗ Same as A. |
| **Logs and alerts** | ✓ Log Analytics is built in. | ~ journald on disk. Add Azure Monitor VM-availability alert and the in-app dead-man alert from 0002. | ~ Log Analytics or `az container logs`. |
| **NSE holidays** | ✓ A cheap no-op: the job starts, sees a holiday and exits in seconds. Needs a calendar in code. | ~ The scheduler must skip holidays. Calendar lives in the repo. | ✓ Same as A, if the container exits. |
| **GitHub Actions deploy via OIDC** | ✓ `azure/login` OIDC, then `az containerapp job update`. | ✓ OIDC, then a tag update on the VM (see Recommendation). | ✓ OIDC, then `az container create`. |
| **Registry without long-lived keys** | ✓ ACR with managed identity. | ✓ ACR with the VM's managed identity. | ✓ ACR with managed identity. |
| **Operational fit with 0002** | ✗ Changes the "one process, local files, systemd" model. | ✓ Matches 0002 exactly. | ✗ Same as A. |

## Recommendation

**Use option B: one Linux VM in Central India, started each trading morning and deallocated each evening, with a Standard static public IP that is reserved from day one.** Estimated cost: about **$18 a month**, the same for paper trading and for live.

Option A is cheaper today ($9.83), but it only works after a code change to the login flow, and it cannot hold the SQLite decision log safely. Once live trading needs the static IP, A costs 2.5× B because of the NAT Gateway. Option C has the same login and storage problems as A. B is the only option that works with the code as it is now.

### Design

- **Compute.** B2als v2, Ubuntu LTS, Docker, systemd. Resize to B2ats v2 after measuring memory.
- **Schedule.**
  - A GitHub Actions workflow on cron starts the VM at about 08:15 IST (02:45 UTC). It runs on the default branch `dev` and reuses the OIDC login. It starts early to absorb GitHub's cron delay.
  - The Azure **auto-shutdown** schedule deallocates the VM at 16:00 IST. This is free and does not depend on GitHub.
  - A guest-OS `shutdown` is **not enough**. It leaves the VM allocated, and billing continues.
- **Holidays.**
  - `deploy/nse-holidays.yaml` in the repo lists weekday holidays and extra sessions. It is updated each year from the NSE circular.
  - The start workflow reads it and skips closed days. The VM stays deallocated and costs nothing.
  - A missed or wrong entry costs one idle VM-day at about $0.20. Auto-shutdown bounds it.
  - Follow-up (code): `run` should refuse to trade on a closed day and exit after a configured session end.
- **Daily login.**
  - **Now:** the operator opens an SSH tunnel to the started VM and runs `jevtrader login` before 09:00. This is the existing documented flow.
  - A systemd wrapper waits for a valid session and starts `run` as soon as one appears. A missed login costs a few hours of idle VM, and the operator notices because the VM is up and nothing trades.
  - **Target state (follow-up issue):** log in on the operator's laptop, where the loopback redirect works natively, and write the session to Key Vault. The service reads it at start. That removes the need for inbound SSH. It needs a token-store backend in `kiteauth`.
  - Container gotcha: `login` binds `127.0.0.1` inside the container, so run it with `--network host`. A published port cannot reach it.
- **Secrets.**
  - `KITE_API_KEY`, `KITE_API_SECRET` and `JEV_API_KEY` live in **Key Vault**, in RBAC mode. The VM's system-assigned managed identity reads them at start into a `0600` `EnvironmentFile` on tmpfs.
  - **GitHub holds no runtime secrets.** CI never sees Kite or Jev credentials. Only three identifiers (client, tenant and subscription IDs) are stored there, and they are not secret.
  - The daily access token stays a `0600` file in the state directory, as in 0002.
- **Storage.**
  - Parquet and the SQLite log are on the VM's managed disk.
  - A nightly job after the close copies the day's Parquet files and an SQLite `.backup` to Blob Storage, using the VM identity.
  - The disk survives deallocation. Local redundancy (LRS) is enough until the data has proven value.
- **Logs and alerts.**
  - journald is the log store at first. Ship to Log Analytics only if needed, because the free 5 GB is easy to exceed with full-mode tick logs.
  - Alerts go to an Azure Monitor action group (email): VM availability, a budget alert at $25 a month, and the in-app dead-man alert (no decision record for N minutes in market hours) from 0002.
- **Static IP.**
  - A Standard static public IPv4 is attached to the NIC from day one. It survives deallocation and is the VM's outbound IP.
  - Protect it with a `CanNotDelete` lock. **Never recreate it:** a new address would need a new Kite whitelist entry.
  - The IP can be registered in the Kite console before live trading exists. Paper trading does not need it.
- **Registry: ACR Basic ($5.07 a month), not GHCR.**
  - The VM pulls with its managed identity (`AcrPull`) and CI pushes over OIDC (`AcrPush`). **No long-lived credential exists anywhere.**
  - GHCR is free for small private use (unverified), but a private image needs a long-lived token with `read:packages` on the VM. A public image would expose the binary.
  - If saving $5 matters more, the deploy step can pass the short-lived `GITHUB_TOKEN` through `az vm run-command`, but the VM would have to be running during every deploy. That is hacky and not recommended.
- **Image tagging and deploy on merge to `main`.**
  - Every merge builds the image once and pushes `jevtrader:<12-char-sha>`. That tag is immutable and is the one deployed. A moving `main` tag may be added for convenience. **`latest` is never deployed.**
  - A release tag (`vX.Y.Z`) is added only when a git tag is cut.
  - The build stamps the git SHA into the binary with `-ldflags`, and the decision log's run record stores it, so every decision is traceable to a build.
  - **Deploy does not touch a running session.** The deploy workflow only sets a VM tag (`jev-image=<sha>`). At each boot, the VM's start script reads its own tag from the instance metadata service and pulls that image before starting `run`. Rollback is a re-tag. Whether tag changes show up in the metadata service quickly enough must be confirmed in #24.
  - Merging during market hours therefore changes nothing until the next morning.

## What the captain must set up in Azure and GitHub

Infrastructure is created in #24 from the captain's own `az login`. CI gets no rights to change infrastructure.

### Azure

1. **Subscription.** Use or create a pay-as-you-go subscription. Register the providers `Microsoft.Compute`, `Microsoft.Network`, `Microsoft.KeyVault`, `Microsoft.ContainerRegistry`, `Microsoft.Storage`, `Microsoft.Insights` and `Microsoft.DevTestLab`. Create a **$25 monthly budget** with email alerts.
2. **Resource group.** `rg-jevtrader-prod` in `centralindia`.
3. **OIDC identities** (an Entra app registration or a user-assigned managed identity). Use two so that the schedule cannot deploy and the deploy cannot start or stop the VM:

   | Identity | Federated credential subject | Role, scoped to the resource group or resource |
   | --- | --- | --- |
   | `gh-jevtrader-deploy` | `repo:sanjeev2040/jev-trader:environment:production` | `AcrPush` on the registry. A tag-write role (`Tag Contributor`) on the VM. |
   | `gh-jevtrader-schedule` | `repo:sanjeev2040/jev-trader:environment:market-hours` | A custom role with only read, start and deallocate on the VM. |

   Using environment subjects, not branch subjects, matters because scheduled workflows run on the **default branch (`dev`)**, while deploys come from `main`.
4. **Key Vault** in RBAC mode with the secrets `kite-api-key`, `kite-api-secret` and `jev-api-key`. Grant the VM's managed identity `Key Vault Secrets User`. The captain enters the secret values in the Azure portal or CLI. They never pass through GitHub or an agent.
5. **Registry.** ACR Basic. Grant the VM identity `AcrPull`.
6. **VM.** B2als v2, Ubuntu LTS, system-assigned managed identity, a 64 GiB Standard SSD, Standard static public IP (with a `CanNotDelete` lock) and an NSG that allows inbound 22 only from the operator's IP. Enable auto-shutdown at 16:00 India Standard Time.
7. **Storage account and container** for backups. Grant the VM identity `Storage Blob Data Contributor` on it.
8. **Monitoring.** An action group with the captain's email, the VM availability alert and the budget alert.
9. **Kite developer console.** Confirm the redirect URL (`http://127.0.0.1:8765/kite/callback`). Add the static IP to the IP whitelist when live trading is being prepared. Check the console for the current rules on changing registered IPs, because they were not verified here.

### GitHub (repository `sanjeev2040/jev-trader`)

1. **Environments.**
   - `production`: the deployment branch is limited to `main`, with the captain as a required reviewer.
   - `market-hours`: limited to the default branch.
2. **Variables** (identifiers only, not secrets): `AZURE_CLIENT_ID` for each identity, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `ACR_NAME`, `VM_NAME` and `RESOURCE_GROUP`, set per environment.
3. **No Azure keys and no Kite or Jev secrets.** Do not create a service principal secret.
4. **Branch protection** on `main`: pull requests from `dev` only, with CI passing (CI is a follow-up issue).
5. **Actions** are enabled, and the plan's included minutes cover about 45 scheduled minutes a month (unverified). Check this in the repository's billing settings.

## Consequences

- **Cost.** About $18 a month, flat. The market-hours schedule saves about $14 of VM compute against a 24/7 VM. The static IP, disk and registry make up most of the bill and do not scale with usage.
- **A VM is operated, not abstracted.** OS patching, Docker and SSH are our job. Use Ubuntu unattended security upgrades, and apply other upgrades only while the VM is deallocated.
- **The scheduler is a single point of failure for the morning start.** GitHub's cron can be late or skipped. A missed start shows up as a VM that is not running when the operator goes to log in. Azure Automation is the fallback scheduler if this happens in practice.
- **Daily login stays a manual, interactive step.** That is intended in 0002. The Key Vault target state removes the SSH tunnel but not the human.
- **Static IP is a permanent commitment.** The IP resource, its lock and its Kite registration become part of the system. A region move means a new IP and a new whitelist entry.
- **Follow-up work.** None of it is done by this report. It needs separate issues:
  - #24: infrastructure code (VM, IP, Key Vault, ACR, storage, auto-shutdown, alerts, identities).
  - A Dockerfile and a CI workflow: unit tests on PRs and the build-and-tag step on merge to `main`.
  - `run`: refuse to trade on closed days, and exit after a configured session end.
  - A start script that waits for the daily login, reads secrets from Key Vault and pulls the tagged image.
  - A holiday calendar file and its yearly update.
  - A Key Vault token store for `kiteauth`.
  - Version stamping in the decision log.
- **When to revisit.** Move to option A only if all of these hold: the token handoff no longer needs a loopback listener, the decision log moves off local SQLite, and Azure offers a static egress IP for jobs that costs under about $10 a month.
- **Unverified items.**
  - The Central India NAT Gateway meter (global price assumed).
  - ACR Basic's included storage.
  - GitHub Actions minute and GHCR allowances.
  - Azure Monitor alert pricing and egress allowances.
  - Kite's rule on how often a registered IP may change.
  - How quickly VM tag changes appear in the metadata service.

## Status

**Accepted (option B)** on 2026-10-02: the repository owner chose the VM option. Implementation is #24; the setup steps are in [deploy/README.md](../deploy/README.md).
