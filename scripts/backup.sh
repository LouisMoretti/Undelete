#!/bin/sh
# Daily backup of the Postgres database: purge of the archives that reached
# BACKUP_RETENTION_DAYS days of age, then pg_dump | gzip to ./backups, with a
# .sha256 sidecar written and verified next to every archive.
# The purge runs first so a failed dump cannot skip it.
#
# NOTE 1: this dump does NOT include ./media (directory for media files).
# Media travel in the archives of scripts/backup-media.sh (full +
# incremental), coupled to the dump of the same day -- pg_dump only backs up
# the database, never the filesystem.
#
# NOTE 2: BACKUP_RETENTION_DAYS is also, in effect, the residual survival
# time of a user's data after /delete_my_data: deleting rows in the database
# does not delete backups already written to disk, which keep containing that
# data until their own purge. This duration must be documented in /privacy to
# be honest with the user about what "deletion" actually means.
set -eu
# BusyBox ash (postgres:16-alpine image) supports pipefail. Without it, a
# failing pg_dump would be masked by gzip's success and produce an empty
# archive presented as valid.
set -o pipefail

: "${MIGRATION_DATABASE_URL:?MIGRATION_DATABASE_URL must be set}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

mkdir -p "$BACKUP_DIR"

# The purge runs BEFORE the dump, not after: with `set -e` a post-dump purge
# never runs when pg_dump fails, and every archive then lives a day longer
# than the number the /delete_my_data confirmation states.
echo "backup: purging archives that reached ${BACKUP_RETENTION_DAYS} days of age"
PURGE_AFTER_MTIME="+$((BACKUP_RETENTION_DAYS - 1))"
if [ "${BACKUP_RETENTION_DAYS}" -le 1 ] 2>/dev/null; then
    PURGE_AFTER_MTIME="+0"
fi
# -mtime compares whole days: -mtime +K matches files strictly older than K+1
# days. Purging with K = RETENTION-1 therefore deletes an archive on the first
# daily run where it is at least RETENTION days old. "About RETENTION days"
# and "as long as this script runs daily" are load-bearing qualifiers: a day
# the job does not run moves every deletion by a day, and the confirmation of
# /delete_my_data states the number with exactly that qualification.
find "$BACKUP_DIR" -name 'undelete-*.sql.gz' -type f -mtime "$PURGE_AFTER_MTIME" -delete
# Sidecars age with their archive (written seconds apart): purge them under
# the same rule, or a deleted archive leaves its checksum behind.
find "$BACKUP_DIR" -name 'undelete-*.sql.gz.sha256' -type f -mtime "$PURGE_AFTER_MTIME" -delete

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${BACKUP_DIR}/undelete-${timestamp}.sql.gz"

echo "backup: dumping to ${dest}"
# Removes any incomplete archive if pg_dump, gzip or the container fails.
# The trap is removed only after the whole pipeline -- checksum included --
# succeeds, so a half-written archive is never presented as valid.
trap 'rm -f "$dest" "${dest}.sha256"' EXIT HUP INT TERM
pg_dump "$MIGRATION_DATABASE_URL" | gzip > "$dest"
echo "backup: checksumming ${dest}"
# Basename-relative sidecar, like scripts/backup-media.sh: the checksum must
# verify from inside any directory holding the pair -- after a transfer, the
# container path that produced it no longer exists.
( cd "$BACKUP_DIR" && sha256sum "$(basename "$dest")" > "$(basename "$dest").sha256" )
# Verify the sidecar just written: a checksum file that does not match its
# archive is worse than none, it would fail every future verification.
( cd "$BACKUP_DIR" && sha256sum -c "$(basename "$dest").sha256" )
trap - EXIT HUP INT TERM

echo "backup: OK ${dest}"
