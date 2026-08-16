#!/usr/bin/env bash
#
# SOBH backup (§37).
#
# Produces an encrypted, verified dump of PostgreSQL and, optionally, mirrors
# object storage. Backups are encrypted before they leave the host, so the
# destination never holds readable data.
#
#   ./scripts/backup.sh daily
#   ./scripts/backup.sh weekly --with-media
#
# Required environment:
#   POSTGRES_DSN          connection string to dump
#   BACKUP_DIR            local staging directory
#   BACKUP_AGE_RECIPIENT  age public key that can decrypt the result
# Optional:
#   BACKUP_REMOTE         rclone remote (e.g. s3:sobh-backups) for offsite copies
#   MINIO_ALIAS           mc alias for the media mirror

set -Eeuo pipefail

TIER="${1:-daily}"
WITH_MEDIA=false
[[ "${2:-}" == "--with-media" ]] && WITH_MEDIA=true

: "${POSTGRES_DSN:?POSTGRES_DSN is required}"
: "${BACKUP_DIR:=/var/backups/sobh}"
: "${BACKUP_AGE_RECIPIENT:?BACKUP_AGE_RECIPIENT is required; backups are never written in the clear}"

# Retention per tier (§37). Monthly copies are kept long enough to satisfy a
# typical annual audit.
case "$TIER" in
  daily)   RETAIN_DAYS=14  ;;
  weekly)  RETAIN_DAYS=90  ;;
  monthly) RETAIN_DAYS=400 ;;
  *) echo "usage: $0 {daily|weekly|monthly} [--with-media]" >&2; exit 2 ;;
esac

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET_DIR="${BACKUP_DIR}/${TIER}"
BASENAME="sobh-${TIER}-${TIMESTAMP}"
DUMP_FILE="${TARGET_DIR}/${BASENAME}.dump"
ENCRYPTED_FILE="${DUMP_FILE}.age"

log()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAILED: $*"; exit 1; }

# A partial backup is worse than none: it looks like protection but cannot be
# restored. Anything left behind by a failure is removed.
cleanup() {
  local status=$?
  if (( status != 0 )); then
    rm -f "$DUMP_FILE" "$ENCRYPTED_FILE"
    log "cleaned up the incomplete backup"
  fi
  exit "$status"
}
trap cleanup EXIT

for tool in pg_dump pg_restore age; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed"
done

mkdir -p "$TARGET_DIR"

log "dumping the database"
# The custom format is compressed and supports parallel restore, which matters
# for the RTO target in §38.
pg_dump --dbname="$POSTGRES_DSN" \
        --format=custom \
        --compress=9 \
        --no-owner \
        --no-privileges \
        --file="$DUMP_FILE"

# Verify before encrypting: a dump that pg_restore cannot list is not a backup.
log "verifying the dump"
pg_restore --list "$DUMP_FILE" >/dev/null || fail "the dump did not verify"

DUMP_SIZE="$(stat -c%s "$DUMP_FILE")"
(( DUMP_SIZE > 1024 )) || fail "the dump is implausibly small (${DUMP_SIZE} bytes)"

log "encrypting"
age --encrypt --recipient "$BACKUP_AGE_RECIPIENT" --output "$ENCRYPTED_FILE" "$DUMP_FILE"
rm -f "$DUMP_FILE"

sha256sum "$ENCRYPTED_FILE" > "${ENCRYPTED_FILE}.sha256"
log "wrote $(basename "$ENCRYPTED_FILE") ($(numfmt --to=iec "$(stat -c%s "$ENCRYPTED_FILE")"))"

if [[ "$WITH_MEDIA" == true ]]; then
  if command -v mc >/dev/null 2>&1 && [[ -n "${MINIO_ALIAS:-}" ]]; then
    log "mirroring object storage"
    # Media objects are immutable once written, so a mirror is enough; the
    # --remove flag is deliberately omitted so a deletion cannot propagate.
    mc mirror --overwrite "${MINIO_ALIAS}/sobh-media" "${TARGET_DIR}/media" || \
      log "WARNING: the media mirror did not complete"
  else
    log "skipping media: mc or MINIO_ALIAS is not configured"
  fi
fi

if [[ -n "${BACKUP_REMOTE:-}" ]] && command -v rclone >/dev/null 2>&1; then
  log "copying offsite to ${BACKUP_REMOTE}"
  # Offsite copies live on separate infrastructure so one compromised host
  # cannot destroy both the system and its backups (§37).
  rclone copy "$ENCRYPTED_FILE" "${BACKUP_REMOTE}/${TIER}/" --checksum
  rclone copy "${ENCRYPTED_FILE}.sha256" "${BACKUP_REMOTE}/${TIER}/" --checksum
fi

log "pruning local backups older than ${RETAIN_DAYS} days"
find "$TARGET_DIR" -maxdepth 1 -name 'sobh-*.age*' -mtime "+${RETAIN_DAYS}" -delete

log "backup complete"
