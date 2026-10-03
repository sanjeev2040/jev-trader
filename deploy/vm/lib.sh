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
