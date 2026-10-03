#!/usr/bin/env bash
# ExecStart of jevtrader-screen@.service: builds the day's watchlist in the image
# that prepare.sh pulled. $1 is the phase, early (about 08:30 IST: news and the
# previous session) or final (about 09:08 IST: adds the pre-open gap). KITE_API_KEY
# and JEV_API_KEY come from systemd's EnvironmentFile and are passed through by
# name, never by value. The Kite session is the one `jevtrader-login` saved.
#
# Whatever happens here, `jevtrader run` still starts: it uses the file if there
# is a good one for today and the static WATCHLIST otherwise.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_conf

phase=${1:?usage: screen.sh early|final}
case "$phase" in early | final) ;; *) die "phase '$phase' is not early or final" ;; esac

if [ "${WATCHLIST_SCREEN:-on}" != on ]; then
  log "WATCHLIST_SCREEN is off in $CONF; not screening"
  exit 0
fi

# One screen at a time: a login at 09:05 releases the early screen just before
# the final timer fires, and two screens would fetch the same candles twice,
# over Kite's 3 requests a second. The second waits here and then finds the
# candles cached. The lock is held by the docker client for as long as it runs.
exec 9> "$RUN_DIR/screen.lock"
flock -w 1500 9 || die "another screen has been running for 25 minutes"

# A late start (the VM came up after 09:08, or the login came late) would run
# the early screen after the final one. The final pass covers everything.
if [ "$phase" = early ] && [ "$(TZ=Asia/Kolkata date +%H%M)" -ge 0908 ]; then
  log "it is past 09:08 IST; skipping the early screen, the final one runs instead"
  exit 0
fi

case "${JEV_MODEL:-}" in "" | *"<"*) die "JEV_MODEL is not set in $CONF" ;; esac
image=$(cat "$RUN_DIR/image")

# SCREEN_ARGS holds extra flags (weights, filters; `jevtrader screen -h`), word-split on purpose.
# shellcheck disable=SC2206
extra=(${SCREEN_ARGS:-})

exec docker run --rm --name "jevtrader-screen-$phase" \
  --user "$APP_UID:$APP_UID" --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges \
  -e KITE_API_KEY -e JEV_API_KEY \
  -v "$STATE_ROOT/data:/data" -v "$STATE_ROOT/state:/state" \
  "$image" screen --phase "$phase" --model "$JEV_MODEL" \
  --data-dir /data --state-dir /state --out-dir /state/watchlist \
  "${extra[@]}"
