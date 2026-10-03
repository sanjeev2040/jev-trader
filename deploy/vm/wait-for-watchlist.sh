#!/usr/bin/env bash
# ExecStartPre of jevtrader-run.service, after wait-for-login.sh: hold `run` back
# until today's watchlist is final, so that it starts with the screened list.
# It never waits past 09:13 IST, two minutes before the open: after that `run`
# starts with whatever file there is (an early one) or, failing that, the static
# WATCHLIST. A login that comes after 09:13 finds the deadline already passed,
# so it does not wait at all.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_conf

if [ "${WATCHLIST_SCREEN:-on}" != on ]; then
  log "WATCHLIST_SCREEN is off; not waiting for a watchlist"
  exit 0
fi

today=$(TZ=Asia/Kolkata date +%F)
file="$STATE_ROOT/state/watchlist/$today.json"
deadline=$(TZ=Asia/Kolkata date -d "$today 09:13:00" +%s)

final() {
  [ -f "$file" ] && jq -e --arg d "$today" '.date == $d and .final == true' "$file" > /dev/null 2>&1
}

waited=0
until final; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    if [ -f "$file" ]; then
      log "no final watchlist by 09:13 IST; starting with the one at $file ($(jq -r '.phase' "$file" 2> /dev/null || echo unreadable))"
    else
      log "no watchlist by 09:13 IST; starting with the static WATCHLIST"
    fi
    exit 0
  fi
  if [ $((waited % 60)) -eq 0 ]; then
    log "waiting for the final watchlist ($file) until 09:13 IST"
  fi
  sleep 5
  waited=$((waited + 5))
done
log "final watchlist found: $(jq -r '[.entries[].key] | join(",")' "$file")"
