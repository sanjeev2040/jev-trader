#!/usr/bin/env bash
# Copies the Parquet files written since the last successful backup, and a
# consistent SQLite .backup of the decision log, to the Blob container, using
# the VM's managed identity (Storage Blob Data Contributor). Run by
# jevtrader-backup.timer shortly after the close, while the VM is still up.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
load_conf

data="$STATE_ROOT/data"
db="$STATE_ROOT/state/decisions.db"
marker="$STATE_ROOT/backup.marker"
base="https://$BACKUP_ACCOUNT.blob.core.windows.net/$BACKUP_CONTAINER"
day=$(TZ=Asia/Kolkata date +%F)
work=$(mktemp -d "$STATE_ROOT/backup.XXXXXX")
trap 'rm -rf "$work"' EXIT

token=$(retry 5 10 aad_token https://storage.azure.com/) || die "cannot get a Storage token"
put() { # put FILE BLOB_NAME
  curl -fsS --max-time 900 --retry 3 -X PUT --upload-file "$1" \
    -H "Authorization: Bearer $token" -H 'x-ms-version: 2022-11-02' \
    -H 'x-ms-blob-type: BlockBlob' -H 'Content-Type: application/octet-stream' \
    "$base/$2" > /dev/null
}

# Parquet files appear atomically (a hidden .tmp file is renamed when closed),
# so anything matching *.parquet is complete. Names contain only
# [A-Za-z0-9=._-], so they need no URL encoding.
started=$(mktemp "$work/started.XXXXXX")
args=(-type f -name '*.parquet' ! -name '.*')
[ -f "$marker" ] && args+=(-newer "$marker")
count=0
while IFS= read -r -d '' f; do
  put "$f" "data/${f#"$data"/}"
  count=$((count + 1))
done < <(find "$data" "${args[@]}" -print0)
log "uploaded $count parquet file(s)"

if [ -f "$db" ]; then
  # .backup is safe while run is writing; WAL mode lets the reader coexist.
  sqlite3 -readonly "$db" ".backup '$work/decisions.db'"
  gzip -f "$work/decisions.db"
  put "$work/decisions.db.gz" "sqlite/decisions-$day.db.gz"
  log "uploaded sqlite/decisions-$day.db.gz"
else
  log "no decision log at $db; skipping the SQLite backup"
fi

touch -r "$started" "$marker" # only after everything above succeeded
