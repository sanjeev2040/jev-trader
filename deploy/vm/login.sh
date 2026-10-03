#!/usr/bin/env bash
# Daily Kite login, run by the operator on the VM (installed as
# /usr/local/bin/jevtrader-login):
#
#   ssh -t -L 8765:127.0.0.1:8765 jevadmin@<vm-ip> sudo jevtrader-login
#
# It prints a Kite URL. Open it in the browser on your own machine and log in;
# Kite redirects to http://127.0.0.1:8765/..., which the SSH tunnel carries to
# the listener started here. The listener binds the VM's loopback, so the
# container uses the host network. jevtrader-run starts by itself once the
# session file appears.
set -euo pipefail
. "$(dirname "$(readlink -f "$0")")/lib.sh"
[ "$(id -u)" -eq 0 ] || die "run with sudo"

# Make sure the image and secrets from this boot are in place.
systemctl start jevtrader-prepare.service
image=$(cat "$RUN_DIR/image")

exec docker run --rm -it \
  --network host \
  --user "$APP_UID:$APP_UID" --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges \
  --env-file "$RUN_DIR/secrets.env" \
  -v "$STATE_ROOT/state:/state" \
  "$image" login --state-dir /state "$@"
