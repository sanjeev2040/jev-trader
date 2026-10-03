# shellcheck shell=bash disable=SC2034
# Shared helpers for the jevtrader VM scripts. Source this file; do not run it.

set -euo pipefail

CONF=/etc/jevtrader/jevtrader.env
STATE_ROOT=/var/lib/jevtrader
RUN_DIR=/run/jevtrader # tmpfs; holds the secrets file and the resolved image
APP_UID=65532          # the non-root user inside the image

log() { echo "jevtrader: $*" >&2; }
die() { log "$*"; exit 1; }

# Non-secret settings written by provision.sh from the Bicep parameters.
load_conf() {
  [ -r "$CONF" ] || die "$CONF is missing; run provision.sh"
  set -a
  # shellcheck disable=SC1090
  . "$CONF"
  set +a
}

# imds PATH: GET from the instance metadata service (link-local, no proxy).
imds() {
  curl -fsS --max-time 10 --noproxy '*' -H 'Metadata: true' "http://169.254.169.254/metadata/$1"
}

# aad_token RESOURCE: an access token for the VM's system-assigned identity.
aad_token() {
  curl -fsS --max-time 10 --noproxy '*' -H 'Metadata: true' --get \
    --data-urlencode "resource=$1" --data 'api-version=2018-02-01' \
    http://169.254.169.254/metadata/identity/oauth2/token | jq -er .access_token
}

# retry N SECONDS COMMAND...: the network and IMDS are not always ready the
# moment the VM boots.
retry() {
  local tries=$1 delay=$2 i
  shift 2
  for ((i = 1; i <= tries; i++)); do
    "$@" && return 0
    [ "$i" -lt "$tries" ] && sleep "$delay"
  done
  return 1
}

# fetch_trading_capital: needs kv_token and KEY_VAULT_NAME (see prepare.sh).
# trading-capital is configuration, not a credential: the total rupee budget the
# bot may have at work. It is optional, so that a vault without it still boots
# (jevtrader then uses its built-in default); a missing secret (404) is skipped,
# but any other failure stops the boot rather than trade on a budget nobody chose.
fetch_trading_capital() {
  local body code value
  body=$(mktemp "$RUN_DIR/capital.XXXXXX")
  code=$(curl -sS --max-time 20 -o "$body" -w '%{http_code}' -H "Authorization: Bearer $kv_token" \
    "https://$KEY_VAULT_NAME.vault.azure.net/secrets/trading-capital?api-version=7.4") ||
    { rm -f "$body"; die "cannot reach Key Vault $KEY_VAULT_NAME to read 'trading-capital'"; }
  case "$code" in
    200) value=$(jq -er .value < "$body") || { rm -f "$body"; die "secret 'trading-capital' has no value"; } ;;
    404) rm -f "$body"; log "secret 'trading-capital' is not set; jevtrader uses its default budget"; return 0 ;;
    *) rm -f "$body"; die "cannot read secret 'trading-capital' from Key Vault $KEY_VAULT_NAME (HTTP $code)" ;;
  esac
  rm -f "$body"
  [[ "$value" =~ ^[0-9]+([.][0-9]+)?$ ]] ||
    die "secret 'trading-capital' is '$value', not a positive number of rupees such as 25000"
  log "trading capital is $value rupees (Key Vault secret trading-capital)"
  printf 'JEVTRADER_TRADING_CAPITAL=%s\n' "$value"
}
