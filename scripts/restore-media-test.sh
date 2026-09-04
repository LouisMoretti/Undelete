#!/bin/sh
# End-to-end restore test of the PAIR (database dump + media archives).
#
# scripts/restore-test.sh already proves a pg_dump is restorable. It proves
# nothing about ./media, and a restored catalogue whose files are missing is a
# database full of dangling references: from Phase 2 on, "the backup is good"
# means both halves come back and agree with each other.
#
# What this script proves, in one run:
#   1. scripts/backup-media.sh produces a FULL archive plus a MANIFEST and a
#      .meta that names the database dump of the same window;
#   2. an INCREMENTAL archive taken afterwards contains exactly the files
#      added since the full -- no more, no less;
#   3. the archives' integrity is verifiable without trusting the transport:
#      sha256 of the archive against its .sha256 sidecar, then `sha256sum -c`
#      of the MANIFEST against the extracted tree;
#   4. restoring in the documented order (DB, then media full, then media
#      incrementals) rebuilds a tree that matches the restored catalogue;
#   5. a media_files row in 'stored' whose file is in NO archive is DETECTED
#      as a missing object, not silently ignored -- this is the discrepancy
#      the reconciliation command of #12 exists to repair;
#   6. a file whose bytes were altered after restore is detected too (sha256
#      mismatch), so "the file exists" is never mistaken for "the file is
#      intact".
#
# Operational safety -- this script NEVER touches:
#   - a Docker volume (no `docker volume rm|prune`, no named volume);
#   - a container it did not create itself (unique timestamped names);
#   - the dev or prod database (it refuses to run if the environment carries
#     an app DSN, cf. the guard below);
#   - the repository's ./media or ./backups directories (everything happens
#     in a `mktemp -d`).
# The only cleanup performed is `docker rm -f` on ITS two containers, plus its
# own temporary directory.
set -eu
# No `set -o pipefail`: like scripts/restore-test.sh, this runs on the host's
# /bin/sh (dash on Debian/Ubuntu), which does not support it. The steps where
# a masked failure would matter -- decompression, SQL replay, extraction --
# are written without pipes.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MIGRATIONS_DIR="$ROOT/bot/internal/storage/migrations"

# --- Guard: refusal of an ambiguous environment ------------------------------
for var in MIGRATION_DATABASE_URL DATABASE_URL; do
    eval "value=\${$var:-}"
    if [ -n "$value" ]; then
        cat >&2 <<EOF
restore-media-test: refusing to run with $var set in the environment.
This test creates both of its disposable databases itself and accepts no
external target: restoring into an existing database would overwrite it.
Re-run in a shell where $var is not exported (e.g. \`env -u $var make test-restore-media\`).
EOF
        exit 1
    fi
done

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
restore-media-test: the Docker daemon is unavailable, no test was run.
This test needs to create two disposable PostgreSQL 16 containers.
No existing container, network, image or volume was modified.
EOF
    exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    echo "restore-media-test: neither sha256sum nor shasum available" >&2
    exit 1
fi

suffix="$$-$(date -u +%s)"
src_container="undelete-media-src-$suffix"
dst_container="undelete-media-dst-$suffix"
src_db="undelete_media_src"
dst_db="undelete_media_dst"
admin_password="restore-media-test-throwaway"

workdir=$(mktemp -d)
# Same permission split as scripts/restore-test.sh: traverse-only on the
# workdir so no other local user can substitute the SQL that is replayed
# later, and a single 0777 write point for the container's foreign uid.
chmod 0711 "$workdir"
mkdir -p "$workdir/backups" "$workdir/media" "$workdir/restored-media"
chmod 0777 "$workdir/backups"

cleanup() {
    docker rm -f "$src_container" >/dev/null 2>&1 || true
    docker rm -f "$dst_container" >/dev/null 2>&1 || true
    rm -rf "$workdir"
}
# The signal traps exit; a handler that only cleans up would return control to
# the script, which would then keep running against a workdir it just deleted
# (and against containers it just removed), burying the real cause under a
# cascade of errors.
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

failures=0
ok() { echo "  [OK]   $1"; }
ko() { echo "  [FAIL] $1" >&2; failures=$((failures + 1)); }

expect_eq() {
    label="$1"
    expected="$2"
    actual="$3"
    if [ "$expected" = "$actual" ]; then
        ok "$label : $actual"
    else
        ko "$label : expected <$expected>, got <$actual>"
    fi
}

start_pg() {
    name="$1"
    dbname="$2"
    docker run --detach --rm \
        --name "$name" \
        --env POSTGRES_USER=postgres \
        --env POSTGRES_PASSWORD="$admin_password" \
        --env POSTGRES_DB="$dbname" \
        --volume "$workdir:/work" \
        postgres:16-alpine >/dev/null

    attempt=0
    until docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d "$dbname" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 60 ]; then
            docker logs "$name" >&2 || true
            echo "restore-media-test: PostgreSQL 16 ($name) never became available" >&2
            exit 1
        fi
        sleep 1
    done
}

psql_in() {
    name="$1"
    dbname="$2"
    shift 2
    docker exec --interactive \
        --env PGPASSWORD="$admin_password" \
        "$name" \
        psql -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d "$dbname" "$@"
}

query() {
    name="$1"
    dbname="$2"
    sql="$3"
    psql_in "$name" "$dbname" -tAc "$sql"
}

create_app_role() {
    psql_in "$1" "$2" -q -c \
        "DO \$\$ BEGIN
             IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'undelete_app') THEN
                 CREATE ROLE undelete_app NOLOGIN;
             END IF;
         END \$\$;"
}

echo "restore-media-test: SOURCE database ($src_container / $src_db)"
start_pg "$src_container" "$src_db"
create_app_role "$src_container" "$src_db"

# --- Migrations ---------------------------------------------------------------
# Applied in file-name order (the zero-padded numeric prefix fixes it), each in
# its own transaction. Fidelity to the Go runner's bookkeeping
# (schema_migrations, version parsing, skip-if-applied) is what
# scripts/restore-test.sh verifies; what this script needs from the schema is
# only that media_files exists with its real constraints.
echo "restore-media-test: applying migrations"
for file in "$MIGRATIONS_DIR"/*.sql; do
    cp "$file" "$workdir/migration.sql"
    psql_in "$src_container" "$src_db" -q --single-transaction < "$workdir/migration.sql"
    rm -f "$workdir/migration.sql"
    echo "  migration applied ($(basename "$file"))"
done

# --- Synthetic tenant and media tree -----------------------------------------
# The tree mirrors the layout produced by the downloader (#10):
#   ./media/<owner_user_id>/<YYYY-MM>/<DD>/<file_unique_id>
# Contents are recognizable text, never real bytes.
echo "restore-media-test: building the synthetic media tree"
owner_id=$(query "$src_container" "$src_db" \
    "INSERT INTO users (telegram_user_id, retention_days) VALUES (999000101, 30) RETURNING id")
day_dir="$owner_id/2026-09/04"
mkdir -p "$workdir/media/$day_dir"

# in_full        : present before the full archive.
# in_incremental : created after it, so only the incremental can carry it.
# never_archived : a row in 'stored' whose file is in NO archive -- the
#                  missing object this test must detect after restore.
printf 'RESTORE-MEDIA-TEST payload one\n' > "$workdir/media/$day_dir/AgACfullone"
printf 'RESTORE-MEDIA-TEST payload two\n' > "$workdir/media/$day_dir/AgACfulltwo"

register_media() {
    # $1 relative_path, $2 sha256, $3 byte_size, $4 message_id
    psql_in "$src_container" "$src_db" -q -c \
        "INSERT INTO media_files (owner_user_id, business_connection_id, chat_id, message_id,
                                  telegram_file_id, telegram_file_unique_id, media_type,
                                  mime_type, byte_size, relative_path, sha256, status)
         VALUES ($owner_id, 'RESTORE-MEDIA-TEST-bc', -100999101, $4,
                 'file-id-$4', 'unique-$4', 'document',
                 'text/plain', $3, '$1', '$2', 'stored')"
}

sha_full_one=$(sha256_of "$workdir/media/$day_dir/AgACfullone")
sha_full_two=$(sha256_of "$workdir/media/$day_dir/AgACfulltwo")
register_media "$day_dir/AgACfullone" "$sha_full_one" \
    "$(wc -c < "$workdir/media/$day_dir/AgACfullone" | tr -d ' ')" 5001
register_media "$day_dir/AgACfulltwo" "$sha_full_two" \
    "$(wc -c < "$workdir/media/$day_dir/AgACfulltwo" | tr -d ' ')" 5002

# The missing object: a catalogue row pointing at a path that was never
# written to disk, and therefore cannot be in any archive. Its sha256 is a
# well-formed but arbitrary value (the CHECK constraint requires 64 hex
# characters), since there are no bytes to hash.
register_media "$day_dir/AgACneverarchived" \
    "0000000000000000000000000000000000000000000000000000000000000000" 42 5003

# --- Database dump: the real scripts/backup.sh -------------------------------
echo "restore-media-test: dumping the database via scripts/backup.sh"
mkdir -p "$workdir/scripts"
cp "$ROOT/scripts/backup.sh" "$workdir/scripts/backup.sh"
docker exec \
    --env MIGRATION_DATABASE_URL="postgres://postgres:${admin_password}@127.0.0.1:5432/${src_db}?sslmode=disable" \
    --env BACKUP_DIR=/work/backups \
    --env BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}" \
    "$src_container" sh /work/scripts/backup.sh

dump=$(find "$workdir/backups" -maxdepth 1 -name 'undelete-*.sql.gz' -type f | head -n 1)
if [ -z "$dump" ]; then
    echo "restore-media-test: no database archive produced by backup.sh" >&2
    exit 1
fi

# --- Media backup: the real scripts/backup-media.sh --------------------------
# The production script itself, not a hand-rolled tar: it is THIS script's
# output whose restorability is on trial.
echo "restore-media-test: FULL media archive via scripts/backup-media.sh"
MEDIA_DIR="$workdir/media" BACKUP_DIR="$workdir/backups" MEDIA_BACKUP_MODE=full \
    sh "$ROOT/scripts/backup-media.sh"

full_archive=$(find "$workdir/backups" -maxdepth 1 -name 'undelete-media-*-full.tar.gz' -type f \
    | sort | tail -n 1)
if [ -z "$full_archive" ]; then
    echo "restore-media-test: no full media archive produced" >&2
    exit 1
fi

# The coupling is the point of the .meta sidecar: an archive that names no
# dump leaves the operator guessing which database state it belongs with.
meta_dump=$(sed -n 's/^db_dump=//p' "${full_archive%.tar.gz}.meta")
expect_eq "full archive coupled to the dump of the window" "$(basename "$dump")" "$meta_dump"

# A file created AFTER the full: an incremental that misses it, or a full that
# retroactively contains it, would both break the chain.
# `-newer` compares mtimes at second granularity on some filesystems, so the
# new file must land strictly after the full's start marker.
sleep 1
printf 'RESTORE-MEDIA-TEST payload three\n' > "$workdir/media/$day_dir/AgACincrone"
sha_incr_one=$(sha256_of "$workdir/media/$day_dir/AgACincrone")
register_media "$day_dir/AgACincrone" "$sha_incr_one" \
    "$(wc -c < "$workdir/media/$day_dir/AgACincrone" | tr -d ' ')" 5004

echo "restore-media-test: INCREMENTAL media archive"
MEDIA_DIR="$workdir/media" BACKUP_DIR="$workdir/backups" MEDIA_BACKUP_MODE=incremental \
    sh "$ROOT/scripts/backup-media.sh"

incr_archive=$(find "$workdir/backups" -maxdepth 1 -name 'undelete-media-*-incremental.tar.gz' -type f \
    | sort | tail -n 1)
if [ -z "$incr_archive" ]; then
    echo "restore-media-test: no incremental media archive produced" >&2
    exit 1
fi

echo "restore-media-test: archive checks"
expect_eq "full archive: 2 file(s) in the manifest" "2" \
    "$(wc -l < "${full_archive%.tar.gz}.manifest" | tr -d ' ')"
expect_eq "incremental archive: only the file added since the full" \
    "$day_dir/AgACincrone" \
    "$(sed 's/^[0-9a-f]\{64\}  //' "${incr_archive%.tar.gz}.manifest")"

# Integrity of the archives themselves, as an offsite copy would be checked
# before even trying to extract: recomputed sha256 against the .sha256
# sidecar, then gzip's own consistency check.
for archive in "$full_archive" "$incr_archive"; do
    name=$(basename "$archive")
    expect_eq "sha256 of $name matches its sidecar" \
        "$(cut -d' ' -f1 < "${archive%.tar.gz}.sha256")" "$(sha256_of "$archive")"
    if gzip -t "$archive" 2>/dev/null; then
        ok "gzip integrity of $name"
    else
        ko "gzip integrity of $name"
    fi
done

# --- Restore, in the documented order ----------------------------------------
# 1. the database, into a distinct and empty target;
# 2. the media, full then incrementals in chronological order;
# 3. reconciliation between the two.
echo "restore-media-test: TARGET database ($dst_container / $dst_db) -- distinct and empty"
start_pg "$dst_container" "$dst_db"
create_app_role "$dst_container" "$dst_db"

dst_tables_before=$(query "$dst_container" "$dst_db" \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'")
if [ "$dst_tables_before" = "0" ]; then
    ok "target empty before restore (public tables): 0"
else
    echo "restore-media-test: the TARGET database is not empty ($dst_tables_before public table(s))." >&2
    echo "restore-media-test: restore ABANDONED, no data was overwritten." >&2
    exit 1
fi

echo "restore-media-test: restoring the database"
gunzip -c "$dump" > "$workdir/restore.sql"
psql_in "$dst_container" "$dst_db" -q < "$workdir/restore.sql"

expect_eq "media_files rows restored" "4" \
    "$(query "$dst_container" "$dst_db" "SELECT count(*) FROM media_files")"

# The restore target for the media is an EMPTY directory, never the source
# tree: extracting over the source would prove nothing (the files are already
# there) and would hide an archive that forgot half its content.
echo "restore-media-test: restoring the media (full, then incremental)"
media_restore_started=$(date -u +%s)
tar -xzf "$full_archive" -C "$workdir/restored-media"
tar -xzf "$incr_archive" -C "$workdir/restored-media"
media_restore_ended=$(date -u +%s)
media_restore_seconds=$((media_restore_ended - media_restore_started))

# MANIFEST verification in the restored tree: this is the check that turns
# "the archive extracted without error" into "every byte came back".
# Verified line by line rather than with `sha256sum -c`: the manifest format is
# deliberately compatible with it (docs/backup-restore.md documents that
# one-liner for operators), but this script must also pass on a host whose
# checksum tool is `shasum`, which has no equivalent flag.
verify_manifest() {
    manifest_file="$1"
    tree="$2"
    bad=0
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        want=${line%% *}
        rel=${line#*  }
        if [ ! -f "$tree/$rel" ] || [ "$(sha256_of "$tree/$rel")" != "$want" ]; then
            bad=$((bad + 1))
        fi
    done < "$manifest_file"
    [ "$bad" -eq 0 ]
}

for archive in "$full_archive" "$incr_archive"; do
    name=$(basename "$archive")
    if verify_manifest "${archive%.tar.gz}.manifest" "$workdir/restored-media"; then
        ok "MANIFEST verified against the restored tree ($name)"
    else
        ko "MANIFEST verified against the restored tree ($name)"
    fi
done

# --- Reconciliation: catalogue vs restored tree ------------------------------
# The same comparison the reconciliation command of #12 performs, done here in
# shell to prove the discrepancy is VISIBLE from the restored pair. Rows are
# read as superuser, which bypasses RLS (FORCE ROW LEVEL SECURITY does not
# apply to a superuser), so no tenant context is needed.
echo "restore-media-test: reconciliation catalogue <-> restored tree"
reconcile() {
    # Prints "<missing> <corrupt>" for the tree passed as $1.
    tree="$1"
    missing=0
    corrupt=0
    query "$dst_container" "$dst_db" \
        "SELECT relative_path || '|' || sha256 FROM media_files
         WHERE status = 'stored' AND relative_path IS NOT NULL
         ORDER BY relative_path" > "$workdir/catalogue"
    while IFS= read -r row; do
        [ -n "$row" ] || continue
        rel=${row%|*}
        want=${row##*|}
        if [ ! -f "$tree/$rel" ]; then
            missing=$((missing + 1))
            echo "    missing object: $rel"
            continue
        fi
        if [ "$(sha256_of "$tree/$rel")" != "$want" ]; then
            corrupt=$((corrupt + 1))
            echo "    corrupted object: $rel"
        fi
    done < "$workdir/catalogue"
    echo "$missing $corrupt"
}

result=$(reconcile "$workdir/restored-media")
echo "$result" | sed '$d'
counts=$(echo "$result" | tail -n 1)
expect_eq "missing objects detected (1 expected: AgACneverarchived)" "1" "${counts% *}"
expect_eq "corrupted objects detected (0 expected)" "0" "${counts#* }"

# Detection of an ALTERED file: "the file is there" must never be enough. A
# restored blob whose bytes drifted (bad transfer, silent bit rot on the
# offsite copy) has to come out as corrupt, not as intact.
printf 'tampered\n' > "$workdir/restored-media/$day_dir/AgACfullone"
result=$(reconcile "$workdir/restored-media")
counts=$(echo "$result" | tail -n 1)
expect_eq "altered file detected as corrupted" "1" "${counts#* }"

echo
echo "restore-media-test: measured media RTO (extraction only): ${media_restore_seconds}s"
if [ "$failures" -eq 0 ]; then
    echo "restore-media-test: SUCCESS -- database and media restore together, and their discrepancies are detected."
    exit 0
fi
echo "restore-media-test: FAILURE -- $failures check(s) failing." >&2
exit 1
