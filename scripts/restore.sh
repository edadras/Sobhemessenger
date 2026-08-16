#!/usr/bin/env bash
#
# SOBH restore (§38).
#
# Restores an encrypted dump produced by backup.sh. The RTO target is one hour,
# so the restore runs in parallel and the script refuses anything that would
# make it slower or riskier.
#
#   ./scripts/restore.sh /var/backups/sobh/daily/sobh-daily-20260816T030000Z.dump.age
#
# Required environment:
#   POSTGRES_DSN              target database
#   BACKUP_AGE_IDENTITY_FILE  age private key file
# Optional:
#   RESTORE_JOBS              parallel restore workers (default: CPU count)
#   ALLOW_NON_EMPTY=true      permit restoring over an existing database

set -Eeuo pipefail

ARCHIVE="${1:-}"
[[ -n "$ARCHIVE" ]] || { echo "usage: $0 <backup.dump.age>" >&2; exit 2; }
[[ -f "$ARCHIVE" ]] || { echo "no such file: $ARCHIVE" >&2; exit 2; }

: "${POSTGRES_DSN:?POSTGRES_DSN is required}"
: "${BACKUP_AGE_IDENTITY_FILE:?BACKUP_AGE_IDENTITY_FILE is required}"
: "${RESTORE_JOBS:=$(nproc 2>/dev/null || echo 4)}"

log()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAILED: $*"; exit 1; }

for tool in psql pg_restore age; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed"
done

# Verify the checksum first: restoring a corrupted dump wastes the recovery
# window and can leave the database half-populated.
if [[ -f "${ARCHIVE}.sha256" ]]; then
  log "verifying the checksum"
  (cd "$(dirname "$ARCHIVE")" && sha256sum --check --status "$(basename "$ARCHIVE").sha256") \
    || fail "the checksum does not match"
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

log "decrypting"
DUMP="${WORK_DIR}/restore.dump"
age --decrypt --identity "$BACKUP_AGE_IDENTITY_FILE" --output "$DUMP" "$ARCHIVE"

log "verifying the dump"
pg_restore --list "$DUMP" >/dev/null || fail "the dump did not verify"

EXISTING_TABLES="$(psql "$POSTGRES_DSN" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" 2>/dev/null || echo 0)"

if [[ "$EXISTING_TABLES" -gt 0 && "${ALLOW_NON_EMPTY:-false}" != "true" ]]; then
  fail "the target database already has ${EXISTING_TABLES} tables; set ALLOW_NON_EMPTY=true to overwrite"
fi

log "restoring with ${RESTORE_JOBS} parallel workers"
START="$(date +%s)"

# --clean --if-exists makes the restore idempotent; --no-owner keeps it working
# when the restore role differs from production's.
pg_restore --dbname="$POSTGRES_DSN" \
           --jobs="$RESTORE_JOBS" \
           --clean --if-exists \
           --no-owner --no-privileges \
           --exit-on-error \
           "$DUMP"

DURATION=$(( $(date +%s) - START ))
log "restore finished in ${DURATION}s"

# A restore that leaves the schema behind the code is not a recovery.
log "verifying the restored schema"
TABLE_COUNT="$(psql "$POSTGRES_DSN" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")"
MIGRATION_COUNT="$(psql "$POSTGRES_DSN" -tAc \
  "SELECT count(*) FROM schema_migrations" 2>/dev/null || echo 0)"

log "restored ${TABLE_COUNT} tables and ${MIGRATION_COUNT} applied migrations"
(( TABLE_COUNT > 50 )) || fail "the restored schema looks incomplete"

if (( DURATION > 3600 )); then
  log "WARNING: the restore exceeded the one-hour RTO target from §38"
fi

log "restore complete — run 'migrate up' before starting the API"
