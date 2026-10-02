#!/usr/bin/env bash
# ExecStart of jevtrader-run.service: paper trading in the image that
# prepare.sh pulled. KITE_API_KEY, KITE_API_SECRET and JEV_API_KEY come from
# systemd's EnvironmentFile and are passed through by name, never by value.
# Live order placement stays off: this always passes --mode=paper.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_conf

[ -n "${WATCHLIST:-}" ] || die "WATCHLIST is not set in $CONF"
case "${JEV_MODEL:-}" in "" | *"<"*) die "JEV_MODEL is not set in $CONF (set the pinned Jev model id)" ;; esac
image=$(cat "$RUN_DIR/image")

exec docker run --rm --name jevtrader \
  --user "$APP_UID:$APP_UID" --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges \
  -e KITE_API_KEY -e KITE_API_SECRET -e JEV_API_KEY \
  -v "$STATE_ROOT/data:/data" -v "$STATE_ROOT/state:/state" \
  "$image" run --mode=paper \
  --watchlist "$WATCHLIST" --model "$JEV_MODEL" \
  --data-dir /data --state-dir /state --log /state/decisions.db
