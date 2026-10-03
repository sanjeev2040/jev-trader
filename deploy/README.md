# Deploying jev-trader to Azure

The design is in [reports/0003-deployment.md](../reports/0003-deployment.md) (option B). In short: one Ubuntu VM in Central India, started on NSE trading mornings by a GitHub Actions cron and deallocated by Azure at 16:00 IST. It costs about $18 a month. Live order placement stays disabled: the VM runs `jevtrader run --mode=paper`.

```
merge dev -> main
   |  release.yml (environment: production, OIDC identity "deploy")
   |  build once -> ACR  jevtrader:<12-char-sha>  -> set VM tag jev-image=<sha>
   v
08:15 IST, Mon-Fri, not an NSE holiday (deploy/nse-holidays.yaml)
   |  market-hours.yml (environment: market-hours, OIDC identity "schedule")
   |  az vm start
   v
VM boot:  jevtrader-prepare   read own tag from IMDS -> pull image from ACR (managed identity)
                              -> secrets from Key Vault into a 0600 file on tmpfs
          jevtrader-run       wait for a valid Kite session -> jevtrader run --mode=paper
you, before 09:00: ssh tunnel + `sudo jevtrader-login`
15:40 IST  jevtrader-backup.timer: Parquet + SQLite .backup -> Blob Storage
16:00 IST  Azure auto-shutdown deallocates the VM (no compute billing)
```

A merge never touches a running session: the deploy step only changes a tag, and the VM reads it at its next boot.

GitHub holds **no secrets**, only identifiers (tenant, subscription, resource names, client ids). Kite and Jev keys live in Key Vault and are read by the VM's managed identity. CI has no rights to change infrastructure.

| Path | What |
| --- | --- |
| `Dockerfile`, `.dockerignore` | static Go binary, non-root (uid 65532), git SHA stamped with `-ldflags` (`jevtrader version`) |
| `.github/workflows/release.yml` | build, push, tag, set the VM tag |
| `.github/workflows/market-hours.yml` | morning start on trading days |
| `deploy/nse-holidays.yaml`, `cmd/tradingday` | the holiday calendar and the check the schedule runs |
| `deploy/vm/` | scripts and systemd units installed on the VM |
| `infra/main.bicep`, `infra/main.bicepparam` | every Azure resource |

Both workflows skip with a notice, and stay green, until the repository variables in step 6 exist.

## One-time setup

You need the Azure CLI (`az`), the GitHub CLI (`gh`), an SSH key pair and an Azure subscription you can create resources in (Owner, or Contributor plus User Access Administrator, on the resource group: the template creates role assignments).

### 1. Subscription

```sh
az login
az account list --output table
az account set --subscription "<subscription name or id>"
for p in Microsoft.Compute Microsoft.Network Microsoft.KeyVault Microsoft.ContainerRegistry \
         Microsoft.Storage Microsoft.Insights Microsoft.DevTestLab Microsoft.ManagedIdentity \
         Microsoft.Consumption; do az provider register --namespace "$p"; done
az vm list-skus --location centralindia --size Standard_B2als_v2 --output table   # must list the size, with no restrictions
```

If the size is restricted for the subscription, request quota in the portal (Subscriptions > Usage + quotas) or pick another size with the `vmSize` parameter.

### 2. Resource group

```sh
az group create --name rg-jevtrader-prod --location centralindia
```

### 3. Parameters

```sh
cp infra/main.bicepparam infra/main.local.bicepparam   # git-ignored
```

Edit `infra/main.local.bicepparam` and replace every `REPLACE_ME`:

- `adminSshPublicKey`: your public key, for example the output of `cat ~/.ssh/id_ed25519.pub`.
- `operatorCidr`: your public IP as `/32`. This is the only address that can reach SSH. When your IP changes, update the NSG rule (see "Operations").
- `watchlist` and `jevModel`: the instruments to paper-trade and the pinned Jev model id (`jevModel` defaults to `jev-1.13.0`). The VM refuses to start `run` while `jevModel` is a placeholder.
- `alertEmail`: where the action group and budget alerts (80% and 100% of $25, plus a 100% forecast) go.
- Optionally `operatorObjectId` (`az ad signed-in-user show --query id -o tsv`): you then get Key Vault Secrets Officer from the template. Otherwise grant it yourself in step 7.

### 4. Deploy the infrastructure

```sh
az deployment group what-if  --resource-group rg-jevtrader-prod --parameters infra/main.local.bicepparam
az deployment group create   --resource-group rg-jevtrader-prod --parameters infra/main.local.bicepparam --name jevtrader
```

This creates the VM (B2als v2, Ubuntu 24.04 LTS, system-assigned identity, 64 GiB Standard SSD), a Standard static public IP with a `CanNotDelete` lock, an NSG that allows SSH only from `operatorCidr`, auto-shutdown at 16:00 India Standard Time, ACR Basic, an empty Key Vault in RBAC mode, a storage account with a `backups` container, the two GitHub identities with their federated credentials and roles, the action group and the budget. The CustomScript extension then installs Docker and the units from `deploy/vm/`. Nothing in the template is secret.

The deploy takes several minutes. Print the identifiers you need next:

```sh
az deployment group show --resource-group rg-jevtrader-prod --name jevtrader --query properties.outputs --output yaml
```

**Register the static IP with Kite now** if you plan to go live later (`publicIpAddress`; step 8). **Never delete or recreate that IP**: a new address needs a new whitelist entry, and Kite limits how often the registered IP may change.

### 5. What the two GitHub identities may do

Created by the template as user-assigned managed identities, with federated credentials on GitHub *environments* (not branches, because the cron runs on the default branch `dev` while releases come from `main`):

| Identity (output) | Federated subject | Roles |
| --- | --- | --- |
| `id-gh-jevtrader-deploy` (`deployClientId`) | `repo:sanjeev2040@22476366/jev-trader@1401087829:environment:production` | `AcrPush` on the registry; `Tag Contributor` on the VM |
| `id-gh-jevtrader-schedule` (`scheduleClientId`) | `repo:sanjeev2040@22476366/jev-trader@1401087829:environment:market-hours` | custom role on the VM: read, instance view, start, deallocate |

GitHub puts the numeric owner and repository IDs in the OIDC subject (`repo:OWNER@OWNER_ID/REPO@REPO_ID:environment:ENV`), so a name-only subject fails with `AADSTS700213`. If you use a fork or another repository name, set `githubRepo` plus `githubOwnerId` and `githubRepoId`, read with:

```bash
gh api users/OWNER --jq .id          # githubOwnerId
gh api repos/OWNER/REPO --jq .id     # githubRepoId
```

The credentials are named `github-production-ids` and `github-market-hours-ids`. Older deployments may still carry name-based `github-production` and `github-market-hours`; they never match and can be deleted (`az identity federated-credential delete`). No client secret exists for either identity.

### 6. GitHub variables and environments

Variables are identifiers, not secrets. Repository level (`REPO=sanjeev2040/jev-trader`):

```sh
REPO=sanjeev2040/jev-trader
OUT() { az deployment group show -g rg-jevtrader-prod -n jevtrader --query "properties.outputs.$1.value" -o tsv; }
gh variable set AZURE_TENANT_ID       --repo $REPO --body "$(OUT tenantId)"
gh variable set AZURE_SUBSCRIPTION_ID --repo $REPO --body "$(OUT subscriptionId)"
gh variable set RESOURCE_GROUP        --repo $REPO --body "$(OUT resourceGroupName)"
gh variable set VM_NAME               --repo $REPO --body "$(OUT vmName)"
gh variable set ACR_NAME              --repo $REPO --body "$(OUT acrName)"
```

Environments, each with its own `AZURE_CLIENT_ID` (a different identity in each):

```sh
gh api -X PUT repos/$REPO/environments/production   -F 'deployment_branch_policy[protected_branches]=false' -F 'deployment_branch_policy[custom_branch_policies]=true'
gh api -X PUT repos/$REPO/environments/market-hours -F 'deployment_branch_policy[protected_branches]=false' -F 'deployment_branch_policy[custom_branch_policies]=true'
gh api -X POST repos/$REPO/environments/production/deployment-branch-policies   -f name=main -f type=branch
gh api -X POST repos/$REPO/environments/market-hours/deployment-branch-policies -f name=dev -f type=branch   # the default branch: scheduled runs use it
gh variable set AZURE_CLIENT_ID --repo $REPO --env production   --body "$(OUT deployClientId)"
gh variable set AZURE_CLIENT_ID --repo $REPO --env market-hours  --body "$(OUT scheduleClientId)"
```

Optional but recommended: in Settings > Environments > `production`, add yourself as a required reviewer, so each release waits for your approval. (Environment protection rules on private repositories need a paid GitHub plan; without it the branch restriction above may also be unavailable and the OIDC subject still protects the identity.) Do **not** create a service-principal secret or any Azure key in GitHub.

Also: Settings > Branches, protect `main` (pull requests from `dev` only, CI passing). Check Settings > Billing that the plan's included Actions minutes cover about 45 scheduled minutes a month (not verified). GitHub disables scheduled workflows after 60 days without repository activity; re-enable them in the Actions tab if that happens.

### 7. Secrets in Key Vault

Enter the values yourself; they never pass through GitHub or an agent. The prompts keep them out of shell history:

```sh
KV=$(OUT keyVaultName)
# only if you did not set operatorObjectId:
az role assignment create --assignee "$(az ad signed-in-user show --query id -o tsv)" \
  --role "Key Vault Secrets Officer" --scope "$(az keyvault show --name "$KV" --query id -o tsv)"

for s in kite-api-key kite-api-secret jev-api-key; do
  read -rsp "$s: " v; echo
  az keyvault secret set --vault-name "$KV" --name "$s" --value "$v" --output none
done
unset v
```

Role assignments can take a few minutes to take effect. The VM identity already holds Key Vault Secrets User.

### 8. Kite developer console

In the [Kite developer console](https://developers.kite.trade/), confirm the app's redirect URL is exactly `http://127.0.0.1:8765/kite/callback` (see [docs/kite-auth.md](../docs/kite-auth.md)). Paper trading needs no IP registration. Before live trading, add the `publicIpAddress` output as the primary static IP and check the console for the current rules on changing registered IPs.

### 9. First release

1. Open a PR from `dev` to `main` and merge it. `release.yml` builds the image, pushes `jevtrader:<12-char-sha>` (and a moving `main` tag, which nothing deploys) to ACR and sets the VM tag. Check the run's summary, or:

   ```sh
   az tag list --resource-id "$(az vm show -g rg-jevtrader-prod -n "$(OUT vmName)" --query id -o tsv)" --query properties.tags
   ```

2. Start the VM for the first time (the cron does this on later days) and watch it come up:

   ```sh
   az vm start -g rg-jevtrader-prod -n "$(OUT vmName)"
   ssh jevadmin@"$(OUT publicIpAddress)" journalctl -u jevtrader-prepare -u jevtrader-run -f
   ```

   `jevtrader-prepare` logs the tag it read (`VM tag jev-image=...`), pulls the image and loads the secrets. `jevtrader-run` then waits for your login.

3. Check the instance metadata service shows the tag. Whether tag changes reach it promptly was not verified when this was written, so confirm it on the first release: on the VM, `curl -s -H Metadata:true --noproxy '*' 'http://169.254.169.254/metadata/instance/compute/tagsList?api-version=2021-02-01&format=json'`. If the tag is missing, `jevtrader-prepare` fails and retries every 30 seconds; if the service shows an older value than the workflow set, wait a few minutes and run `sudo systemctl restart jevtrader-prepare jevtrader-run`. The VM keeps only the most recent image, so a rollback pulls the older one from ACR again.

4. Log in (the runbook below). Trading starts by itself once a valid session exists.

## Daily runbook

The VM starts at about 08:15 IST (GitHub's cron can be a few minutes late) on NSE trading days. Check that "Market hours" ran in the Actions tab, or that the VM is running. If it did not start, run the workflow by hand (Actions > Market hours > Run workflow) or `az vm start`.

**Before 09:00 IST**, from your own machine:

```sh
ssh -t -L 8765:127.0.0.1:8765 jevadmin@<vm-ip> sudo jevtrader-login
```

1. It prints a Kite login URL. Open it in the browser on your machine and log in, including 2FA.
2. Kite redirects to `http://127.0.0.1:8765/kite/callback`. The SSH tunnel carries that to the listener on the VM, which saves the day's session.
3. Within about 15 seconds `jevtrader-run` notices the session and starts. Check:

   ```sh
   ssh jevadmin@<vm-ip> 'systemctl status jevtrader-run --no-pager; journalctl -u jevtrader-run -n 30 --no-pager'
   ```

If you log in late, trading starts when you do, inside the risk gate's entry window. If you never log in, nothing trades and the VM is deallocated at 16:00 anyway; the cost is one idle VM-day, about $0.20. If `run` stops mid-day with a token error, run `jevtrader-login` again: systemd restarts `run` every 30 seconds until it starts.

Emergency stop: `ssh jevadmin@<vm-ip> sudo touch /var/lib/jevtrader/state/KILL` trips the kill switch (see [docs/run-and-replay.md](../docs/run-and-replay.md)).

At 15:40 IST the backup timer copies the new Parquet files and a consistent SQLite backup to the `backups` container (`data/...` and `sqlite/decisions-<date>.db.gz`). Inspect with `az storage blob list --account-name "$(OUT storageAccountName)" --container-name backups --auth-mode login --output table`. Restoring is the reverse: download the blobs and put the files under `/var/lib/jevtrader/{data,state}` owned by uid 65532.

## Operations

- **Rollback.** Actions > Release > Run workflow on `main`, with `rollback_sha` set to an earlier 12-character tag (`az acr repository show-tags --name <acr> --repository jevtrader`). It checks the image exists and re-points the VM tag. It takes effect at the next boot; to apply it today, `sudo systemctl restart jevtrader-prepare jevtrader-run` on the VM.
- **Holiday calendar.** When NSE publishes the next year's circular (usually December), add its weekday holidays to [`deploy/nse-holidays.yaml`](nse-holidays.yaml), mark them `verified: true` and add the year to `years_complete`. Until a year is complete the VM still starts on its weekdays and the workflow logs a warning. 2027 holds only a few unverified dates. Check a date with `go run ./cmd/tradingday -date 2026-11-10`. A weekend special session needs a manual start: Run workflow with `force`. Muhurat trading is an evening session and falls after the 16:00 shutdown.
- **Change the watchlist or model.** Edit `infra/main.local.bicepparam` and redeploy (start the VM first: the extension cannot run on a deallocated VM), or edit `/etc/jevtrader/jevtrader.env` on the VM and `sudo systemctl restart jevtrader-run`. A redeploy overwrites hand edits.
- **Redeploy the infrastructure** (for example after changing `deploy/vm/`): `az vm start`, run the `az deployment group create` from step 4, and let the auto-shutdown deallocate it again. The VM's `jev-image` tag is not part of the template, so a redeploy leaves it alone.
- **Your IP changed.** `az network nsg rule update -g rg-jevtrader-prod --nsg-name nsg-jevtrader -n ssh-from-operator --source-address-prefixes <new-ip>/32`.
- **Resize.** After measuring resident memory over a recorded session, run `az vm deallocate` and `az vm resize --size Standard_B2ats_v2`, then set `vmSize` in your parameters so a later redeploy agrees.
- **Patching.** Ubuntu's unattended security upgrades are on. Apply other upgrades only while the VM is up in the morning, never mid-session, and reboot afterwards.
- **Logs.** The journal persists across the nightly deallocation: `journalctl -u jevtrader-run --since yesterday`.
- **Cost.** About $18 a month, flat: VM 157.5 h x $0.0246, 64 GiB disk, static IP and ACR Basic. The budget alerts at 80% and 100% of $25; it never stops anything.

## Troubleshooting

| Symptom | Look at |
| --- | --- |
| Workflow summary says "Azure is not configured yet" | the repository variables in step 6 |
| `azure/login` fails with `AADSTS70021` or "No matching federated identity record" | the environment name, `githubRepo` and the numeric `githubOwnerId`/`githubRepoId` must match the credential subject exactly (`AADSTS700213` shows the subject GitHub presented); check `AZURE_CLIENT_ID` is set on the *environment*, not only on the repository |
| `jevtrader-prepare` says "no valid jev-image tag" | no release has run yet; merge to `main` or run the Release workflow |
| `jevtrader-prepare` cannot read a secret | the secret is not set, or the Key Vault role assignment has not propagated yet (it retries every 30 s) |
| `jevtrader-run` keeps restarting | `journalctl -u jevtrader-run`: usually Kite rejecting the session; log in again |
| Bicep redeploy fails on the extension | the VM was deallocated; start it and retry |
| Deleting the resource group fails | the static IP lock is working; remove the lock first, knowing the IP will be lost |
