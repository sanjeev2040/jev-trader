#!/usr/bin/env bash
# One-time (and idempotent) setup of the trading VM: Docker, the boot and
# backup scripts, and their systemd units. Bicep runs it through the
# CustomScript extension; you can also copy this directory to the VM and run
# `sudo ./provision.sh` after exporting the variables below.
#
# Required environment (non-secret values from the Bicep deployment):
#   ACR_LOGIN_SERVER  KEY_VAULT_NAME  BACKUP_ACCOUNT  BACKUP_CONTAINER
#   WATCHLIST         JEV_MODEL
set -euo pipefail

src=$(cd "$(dirname "$0")" && pwd)
for v in ACR_LOGIN_SERVER KEY_VAULT_NAME BACKUP_ACCOUNT BACKUP_CONTAINER WATCHLIST JEV_MODEL; do
  [ -n "${!v:-}" ] || { echo "provision: $v is not set" >&2; exit 1; }
done
[ "$(id -u)" -eq 0 ] || { echo "provision: run as root" >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=600 update -qq
apt-get -o DPkg::Lock::Timeout=600 install -y -qq docker.io jq curl sqlite3 ca-certificates

# Docker: rotate its own logs (the trading service logs to journald).
mkdir -p /etc/docker
cat > /etc/docker/daemon.json.new <<'JSON'
{"log-driver": "json-file", "log-opts": {"max-size": "10m", "max-file": "3"}}
JSON
if ! cmp -s /etc/docker/daemon.json.new /etc/docker/daemon.json 2> /dev/null; then
  mv /etc/docker/daemon.json.new /etc/docker/daemon.json
  systemctl restart docker
else
  rm /etc/docker/daemon.json.new
fi
systemctl enable --now docker

# Keep the journal across reboots: the VM is deallocated every evening, and
# yesterday's logs are how you debug a bad day.
mkdir -p /etc/systemd/journald.conf.d /var/log/journal
printf '[Journal]\nStorage=persistent\n' > /etc/systemd/journald.conf.d/jevtrader.conf
systemctl restart systemd-journald

# Directories. The container runs as uid 65532 and owns data and state.
install -d -m 0755 /etc/jevtrader /usr/local/lib/jevtrader
install -d -m 0755 /var/lib/jevtrader
install -d -m 0750 -o 65532 -g 65532 /var/lib/jevtrader/data /var/lib/jevtrader/state

# Non-secret settings, shell-quoted so a watchlist with spaces survives.
{
  for v in ACR_LOGIN_SERVER KEY_VAULT_NAME BACKUP_ACCOUNT BACKUP_CONTAINER WATCHLIST JEV_MODEL; do
    printf '%s=%q\n' "$v" "${!v}"
  done
} > /etc/jevtrader/jevtrader.env
chmod 0644 /etc/jevtrader/jevtrader.env

for f in lib.sh prepare.sh wait-for-login.sh run.sh login.sh backup.sh; do
  install -m 0755 "$src/$f" "/usr/local/lib/jevtrader/$f"
done
ln -sf /usr/local/lib/jevtrader/login.sh /usr/local/bin/jevtrader-login

for u in jevtrader-prepare.service jevtrader-run.service jevtrader-backup.service jevtrader-backup.timer; do
  install -m 0644 "$src/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
# prepare and run start at every boot; they are not started now because the
# first start needs an image tag and a Kite login.
systemctl enable jevtrader-prepare.service jevtrader-run.service
systemctl enable --now jevtrader-backup.timer
echo "provision: done"
