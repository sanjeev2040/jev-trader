#!/usr/bin/env bash
# ExecStartPre of jevtrader-run.service: block until a Kite session exists and
# has not expired, so `jevtrader run` starts as soon as the operator has logged
# in. It does not talk to Kite; `run` verifies the token itself at startup and
# exits (and systemd retries) if Kite rejects it.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

session="$STATE_ROOT/state/kite-session.json"
valid() {
  local expires
  [ -f "$session" ] || return 1
  expires=$(jq -er '.expires_at' "$session" 2> /dev/null) || return 1
  expires=$(date -d "$expires" +%s 2> /dev/null) || return 1
  [ "$expires" -gt "$(date +%s)" ]
}

waited=0
until valid; do
  # Every 5 minutes, so the journal shows why nothing is trading.
  if [ $((waited % 300)) -eq 0 ]; then
    log "waiting for a Kite session: open an SSH tunnel and run 'sudo jevtrader-login' (see deploy/README.md)"
  fi
  sleep 15
  waited=$((waited + 15))
done
log "Kite session found; starting run"
