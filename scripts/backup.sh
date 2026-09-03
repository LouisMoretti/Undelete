#!/bin/sh
# Daily backup of the Postgres database: pg_dump | gzip to
# ./backups, then purge of archives older than BACKUP_RETENTION_DAYS.
#
# NOTE 1: this dump does NOT include ./media (directory for media files).
# In Phase 1 this directory is empty (no media handling), but from
# Phase 2 a separate backup of ./media will be required -- pg_dump only
# backs up the database, never the filesystem.
#
# NOTE 2: BACKUP_RETENTION_DAYS is also, in effect, the residual survival
# time of a user's data after a future /delete_my_data
# (Phase 2+): deleting rows in the database does not delete backups already
# written to disk, which will keep containing that data until their own
# purge. This duration must be documented in /privacy to be honest with
# the user about what "deletion" actually means.
set -eu
# BusyBox ash (postgres:16-alpine image) supports pipefail. Without it, a
# failing pg_dump would be masked by gzip's success and produce an empty
# archive presented as valid.
set -o pipefail

: "${MIGRATION_DATABASE_URL:?MIGRATION_DATABASE_URL must be set}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

mkdir -p "$BACKUP_DIR"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${BACKUP_DIR}/undelete-${timestamp}.sql.gz"

echo "backup: dumping to ${dest}"
# Removes any incomplete archive if pg_dump, gzip or the container fails.
# The trap is removed only after the whole pipeline succeeds.
trap 'rm -f "$dest"' EXIT HUP INT TERM
pg_dump "$MIGRATION_DATABASE_URL" | gzip > "$dest"
trap - EXIT HUP INT TERM

echo "backup: purging archives older than ${BACKUP_RETENTION_DAYS} days"
find "$BACKUP_DIR" -name 'undelete-*.sql.gz' -type f -mtime "+${BACKUP_RETENTION_DAYS}" -delete

echo "backup: done"
