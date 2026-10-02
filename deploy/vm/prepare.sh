#!/usr/bin/env bash
# Runs at every boot before the trading service (jevtrader-prepare.service):
#   1. reads this VM's jev-image tag from the instance metadata service,
#   2. logs in to ACR with the VM's managed identity and pulls that image,
#   3. fetches the secrets from Key Vault into a 0600 file on tmpfs.
# Nothing here is stored on disk except the last good image tag.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_conf
umask 077

# 1. Which image? The deploy workflow sets the VM tag jev-image=<12-char sha>.
# A tag changed while the VM was deallocated is visible here at the next start.
# The value is always logged; if IMDS is slow or the tag is missing, the last
# good image is the fallback.
read_tag() {
  local value
  value=$(imds 'instance/compute/tagsList?api-version=2021-02-01&format=json' |
    jq -r '.[] | select(.name == "jev-image") | .value' | head -n1) || return 1
  [ -n "$value" ] && printf '%s\n' "$value"
}
tag=$(retry 6 10 read_tag || true)
if [[ ! "$tag" =~ ^[0-9a-f]{12}$ ]]; then
  log "VM tag jev-image is '${tag}', not a 12-character sha"
  tag=""
fi
last_good_file="$STATE_ROOT/last-good-image"
last_good=""
[ -r "$last_good_file" ] && last_good=$(cat "$last_good_file")
if [ -z "$tag" ]; then
  [ -n "$last_good" ] || die "no valid jev-image tag and no previous image: merge to main (or run the Release workflow) so the tag is set"
  log "falling back to the last good image $last_good"
  tag=$last_good
fi
log "VM tag jev-image=$tag"

# 2. Pull from ACR with the managed identity (the VM holds AcrPull).
acr_login() {
  local aad refresh
  aad=$(aad_token https://management.azure.com/)
  refresh=$(curl -fsS --max-time 20 -X POST "https://$ACR_LOGIN_SERVER/oauth2/exchange" \
    --data-urlencode grant_type=access_token --data-urlencode "service=$ACR_LOGIN_SERVER" \
    --data-urlencode "access_token=$aad" | jq -er .refresh_token)
  printf '%s' "$refresh" | docker login "$ACR_LOGIN_SERVER" -u 00000000-0000-0000-0000-000000000000 --password-stdin > /dev/null
}
retry 5 10 acr_login || die "cannot log in to $ACR_LOGIN_SERVER"
image="$ACR_LOGIN_SERVER/jevtrader:$tag"
if ! retry 3 10 docker pull --quiet "$image" > /dev/null; then
  [ -n "$last_good" ] && [ "$tag" != "$last_good" ] || die "cannot pull $image"
  log "cannot pull $image; falling back to $last_good"
  tag=$last_good
  image="$ACR_LOGIN_SERVER/jevtrader:$tag"
  retry 3 10 docker pull --quiet "$image" > /dev/null || die "cannot pull $image"
fi
printf '%s\n' "$tag" > "$last_good_file"
printf '%s\n' "$image" > "$RUN_DIR/image"
log "image $image is ready"
docker image ls --format '{{.Repository}}:{{.Tag}}' "$ACR_LOGIN_SERVER/jevtrader" |
  grep -vxF "$image" | xargs -r docker rmi > /dev/null 2>&1 || true
docker image prune --force > /dev/null || true

# 3. Secrets: Key Vault -> a 0600 file on tmpfs, read by systemd as an
# EnvironmentFile and by jevtrader-login. Never written to disk or logged.
kv_token=$(retry 5 10 aad_token https://vault.azure.net) || die "cannot get a Key Vault token"
fetch_secret() { # fetch_secret SECRET_NAME ENV_NAME
  local value
  value=$(curl -fsS --max-time 20 -H "Authorization: Bearer $kv_token" \
    "https://$KEY_VAULT_NAME.vault.azure.net/secrets/$1?api-version=7.4" | jq -er .value) ||
    die "cannot read secret '$1' from Key Vault $KEY_VAULT_NAME (is it set, and does the VM identity hold Key Vault Secrets User?)"
  [ -n "$value" ] || die "secret '$1' is empty"
  case "$value" in *$'\n'* | *$'\r'*) die "secret '$1' contains a newline" ;; esac
  printf '%s=%s\n' "$2" "$value"
}
tmp=$(mktemp "$RUN_DIR/secrets.env.XXXXXX")
trap 'rm -f "$tmp"' EXIT
{
  fetch_secret kite-api-key KITE_API_KEY
  fetch_secret kite-api-secret KITE_API_SECRET
  fetch_secret jev-api-key JEV_API_KEY
} > "$tmp"
chmod 0600 "$tmp"
mv "$tmp" "$RUN_DIR/secrets.env"
trap - EXIT
log "secrets loaded into $RUN_DIR/secrets.env"
