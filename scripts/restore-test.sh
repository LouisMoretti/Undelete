#!/bin/sh
# End-to-end PostgreSQL restore test: a backup never restored is not a
# verified backup. This script creates a disposable SOURCE database, applies
# migrations and synthetic data to it, produces an archive with
# scripts/backup.sh, then restores it into an explicitly DISTINCT and EMPTY
# TARGET database before comparing the two.
#
# Operational safety -- this script NEVER touches:
#   - a Docker volume (no `docker volume rm|prune`, no named volume);
#   - a container it did not create itself (unique timestamped names);
#   - the dev or prod database (it refuses to run if the environment
#     carries an app DSN, cf. guard below);
#   - the repository's ./backups directory (the archive goes into a mktemp -d).
# The only cleanup performed is `docker rm -f` on ITS two containers.
set -eu
# No `set -o pipefail` here, unlike scripts/backup.sh: that one runs in the
# BusyBox ash of the postgres image, which supports it, whereas this script
# runs on the host machine's /bin/sh (dash on Debian/Ubuntu), which does not.
# The steps where a failed pipeline stage would be masked by the next one's
# success -- decompression and especially SQL replay -- are therefore written
# without pipes: each command is tested on its own.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MIGRATIONS_DIR="$ROOT/bot/internal/storage/migrations"

# --- Guard: refusal of an ambiguous environment ------------------------------
# The restore target must be a disposable database created here, never a
# database supplied by the caller. If the environment already carries a
# project DSN, we cannot rule out that it points at the dev or prod database:
# we stop rather than guess.
for var in MIGRATION_DATABASE_URL DATABASE_URL; do
    eval "value=\${$var:-}"
    if [ -n "$value" ]; then
        cat >&2 <<EOF
restore-test: refusing to run with $var set in the environment.
This test creates both of its disposable databases itself and accepts no
external target: restoring into an existing database would overwrite it.
Re-run in a shell where $var is not exported (e.g. \`env -u $var make test-restore\`).
EOF
        exit 1
    fi
done

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
restore-test: the Docker daemon is unavailable, no test was run.
This test needs to create two disposable PostgreSQL 16 containers.
No existing container, network, image or volume was modified.
EOF
    exit 1
fi

# --- Identity of disposable resources ----------------------------------------
# Unique suffix (PID + epoch): two concurrent or successive runs never step
# on each other, and no name can collide with a pre-existing container. The
# script is therefore re-runnable as-is.
suffix="$$-$(date -u +%s)"
src_container="undelete-restore-src-$suffix"
dst_container="undelete-restore-dst-$suffix"
src_db="undelete_restore_src"
dst_db="undelete_restore_dst"
admin_password="restore-test-throwaway"

workdir=$(mktemp -d)
# The container writes its archive into this mounted directory under a uid
# that is not the host's: a write path must therefore be opened for it. It is
# NOT opened on all of $workdir. That one contains the SQL replayed later
# (migration.sql, restore.sql): a 0777 there would let any local user of the
# same host substitute that SQL between its production and its replay, and the
# test would then pass its verdict on something other than the archive.
#   0711 on $workdir         -> traverse only: no listing or creation by a third party
#   0777 on $workdir/backups -> the only write point the container needs
# Directory created just now by mktemp and removed by the trap, never a
# repository path.
chmod 0711 "$workdir"
mkdir -p "$workdir/backups"
chmod 0777 "$workdir/backups"

cleanup() {
    docker rm -f "$src_container" >/dev/null 2>&1 || true
    docker rm -f "$dst_container" >/dev/null 2>&1 || true
    rm -rf "$workdir"
}
trap cleanup EXIT HUP INT TERM

failures=0
ok() { echo "  [OK]   $1"; }
ko() { echo "  [FAIL] $1" >&2; failures=$((failures + 1)); }

# Compares an expected value to an observed value and records a verdict.
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

# --- Starting a disposable PostgreSQL 16 -------------------------------------
# `--rm` + no named `--volume`: the data lives in the container's ephemeral
# layer and disappears with it. No `--publish` either: all access (psql,
# pg_isready, backup.sh) goes through `docker exec` to 127.0.0.1 INSIDE the
# container, so publishing a port would add nothing and would expose on the
# host a superuser whose password is a literal in this script.
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

    # Wait on 127.0.0.1 and not on the Unix socket: the official entrypoint
    # first starts a temporary server reachable by socket only.
    # A pg_isready on the socket would therefore succeed before the real TCP
    # listener is up.
    attempt=0
    until docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d "$dbname" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 60 ]; then
            docker logs "$name" >&2 || true
            echo "restore-test: PostgreSQL 16 ($name) never became available" >&2
            exit 1
        fi
        sleep 1
    done
}

# Non-interactive psql in a given container, given database. ON_ERROR_STOP is
# essential: without it psql continues after an error and exits 0.
psql_in() {
    name="$1"
    dbname="$2"
    shift 2
    docker exec --interactive \
        --env PGPASSWORD="$admin_password" \
        "$name" \
        psql -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d "$dbname" "$@"
}

# Scalar query: raw output, without header or alignment.
query() {
    name="$1"
    dbname="$2"
    sql="$3"
    psql_in "$name" "$dbname" -tAc "$sql"
}

echo "restore-test: SOURCE database ($src_container / $src_db)"
start_pg "$src_container" "$src_db"

# Migrations 0002 and 0003 issue explicit GRANTs on the app role: it must
# exist before replaying them. db/init is not used here (this test validates
# the backup only, not provisioning), so the role is created with minimum
# requirements. NOLOGIN and NO password: nobody connects to it during this
# test, so a password would protect nothing and would have to be concatenated
# into this SQL. Roles are not in a plain pg_dump (they are global objects),
# so the TARGET must create it too before restoration.
create_app_role() {
    psql_in "$1" "$2" -q -c \
        "DO \$\$ BEGIN
             IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'undelete_app') THEN
                 CREATE ROLE undelete_app NOLOGIN;
             END IF;
         END \$\$;"
}
create_app_role "$src_container" "$src_db"

# --- Migrations: faithful replica of the Go runner ---------------------------
# storage.RunMigrations (bot/internal/storage/db.go) creates schema_migrations
# with THIS exact DDL, sorts files by name (the numeric prefix therefore fixes
# the order), skips versions already present, and applies each migration WITH
# its version INSERT in A SINGLE transaction. All four points are reproduced
# here: if this script diverged from the runner, it would validate a schema
# the binary never produces.
echo "restore-test: applying migrations (numeric order)"
psql_in "$src_container" "$src_db" -q -c \
    "CREATE TABLE IF NOT EXISTS schema_migrations (
         version    INT PRIMARY KEY,
         applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
     )"

# Like the Go runner, ALL versions are validated before any DDL is executed:
# strictly positive integer, and no duplicates. Without this preliminary pass,
# two files with equivalent versions ("10_x.sql" and "0010_y.sql") would pass
# here only to fail mid-way on the schema_migrations primary key, whereas the
# binary refuses to start outright.
parse_version() {
    # "0001_init.sql" -> 1, like parseVersion() on the Go side.
    case "$1" in
        *_*) ;;
        *) return 1 ;;
    esac
    prefix="${1%%_*}"
    case "$prefix" in
        '' | *[!0-9]*) return 1 ;;
    esac
    # Leading zeros stripped by sed, NOT by $((10#$prefix)): the base#number
    # notation is a bashism dash refuses, and $((0010)) alone would be read as
    # octal. "0000" -> "" -> rejected by the > 0 test on the calling side.
    printf '%s' "$prefix" | sed 's/^0*//'
}

expected_versions=""
for file in "$MIGRATIONS_DIR"/*.sql; do
    base=$(basename "$file")
    # `|| true`: parse_version signals an invalid name with a non-zero status
    # and empty output, which the test -z below catches. Without it, set -e
    # would exit on the assignment before the explicit error message.
    version=$(parse_version "$base" || true)
    if [ -z "$version" ] || [ "$version" -le 0 ]; then
        echo "restore-test: invalid migration name: $base (expected format <version>_<name>.sql, version > 0)" >&2
        exit 1
    fi
    for seen in $expected_versions; do
        if [ "$seen" = "$version" ]; then
            echo "restore-test: duplicate migration version $version (last file: $base)" >&2
            exit 1
        fi
    done
    expected_versions="${expected_versions}${version}
"
    # Migration + registration of its version in the same transaction:
    # a migration that fails halfway must not be marked applied.
    # Going through a file rather than a pipe so that a failure to build the
    # script is visible (cf. the absence of pipefail at the top).
    cp "$file" "$workdir/migration.sql"
    printf '\nINSERT INTO schema_migrations (version) VALUES (%s);\n' "$version" \
        >> "$workdir/migration.sql"
    psql_in "$src_container" "$src_db" -q --single-transaction < "$workdir/migration.sql"
    rm -f "$workdir/migration.sql"
    echo "  migration $version applied ($base)"
done
expected_versions=$(printf '%s' "$expected_versions" | sort -n)

# --- Synthetic data ----------------------------------------------------------
# Fictional, recognizable values (RESTORE-TEST prefix, Telegram identifiers
# outside the real range): if they leaked into a real database, they would be
# immediately identifiable as test data.
echo "restore-test: inserting synthetic data"
psql_in "$src_container" "$src_db" -q --single-transaction <<'SQL'
INSERT INTO users (telegram_user_id, retention_days) VALUES
    (999000001, 7),
    (999000002, 30);

INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
SELECT 'RESTORE-TEST-bc-1', id, TRUE, TRUE FROM users WHERE telegram_user_id = 999000001;

INSERT INTO chats (owner_user_id, business_connection_id, chat_id, title, username, type)
SELECT id, 'RESTORE-TEST-bc-1', -100999001, 'RESTORE-TEST chat', 'restore_test', 'private'
FROM users WHERE telegram_user_id = 999000001;

INSERT INTO messages (owner_user_id, business_connection_id, chat_id, message_id,
                      from_user_id, from_display, message_type, text_content, telegram_date)
SELECT id, 'RESTORE-TEST-bc-1', -100999001, 4242, 999000002, 'RESTORE-TEST sender',
       'text', 'RESTORE-TEST canary content', 1700000000
FROM users WHERE telegram_user_id = 999000001;

INSERT INTO notification_outbox (owner_user_id, owner_telegram_user_id, business_connection_id,
                                 chat_id, message_id, event_type, payload_text)
SELECT id, 999000001, 'RESTORE-TEST-bc-1', -100999001, 4242, 'deleted',
       'RESTORE-TEST canary payload'
FROM users WHERE telegram_user_id = 999000001;
SQL

# SOURCE fingerprints, taken BEFORE the dump: these are what the TARGET must
# reproduce identically.
src_users=$(query "$src_container" "$src_db" "SELECT count(*) FROM users")
src_connections=$(query "$src_container" "$src_db" "SELECT count(*) FROM business_connections")
src_chats=$(query "$src_container" "$src_db" "SELECT count(*) FROM chats")
src_messages=$(query "$src_container" "$src_db" "SELECT count(*) FROM messages")
src_outbox=$(query "$src_container" "$src_db" "SELECT count(*) FROM notification_outbox")
src_canary=$(query "$src_container" "$src_db" \
    "SELECT text_content FROM messages WHERE chat_id = -100999001 AND message_id = 4242")

# --- Backup: the real scripts/backup.sh --------------------------------------
# The production script itself is executed, not a rewritten pg_dump: it is
# THIS script whose output must be proven restorable. BACKUP_DIR points at
# the mounted mktemp -d, never at the repository's ./backups.
echo "restore-test: backing up via scripts/backup.sh"
# Copy (not a direct mount of the repository): the container only sees a
# temporary directory, never the project tree.
mkdir -p "$workdir/scripts"
cp "$ROOT/scripts/backup.sh" "$workdir/scripts/backup.sh"
docker exec \
    --env MIGRATION_DATABASE_URL="postgres://postgres:${admin_password}@127.0.0.1:5432/${src_db}?sslmode=disable" \
    --env BACKUP_DIR=/work/backups \
    --env BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}" \
    "$src_container" sh /work/scripts/backup.sh

archive=$(find "$workdir/backups" -name 'undelete-*.sql.gz' -type f | head -n 1)
if [ -z "$archive" ]; then
    echo "restore-test: no archive produced by backup.sh" >&2
    exit 1
fi
echo "restore-test: archive $(basename "$archive") ($(wc -c < "$archive") bytes)"

echo "restore-test: archive checks"
if gzip -t "$archive" 2>/dev/null; then
    ok "gzip integrity of the archive"
else
    ko "gzip integrity of the archive"
fi

# --- Restoring into a distinct and empty TARGET ------------------------------
# Second container, second name, second database: structurally, the restore
# cannot land in the source or in an existing database.
echo "restore-test: TARGET database ($dst_container / $dst_db) -- distinct and empty"
start_pg "$dst_container" "$dst_db"
create_app_role "$dst_container" "$dst_db"

# Proof that the target is truly empty before restore: no tables.
# BLOCKING verdict and not a simple failure counter: "distinct and empty
# target" is the central guarantee of this recipe. If it does not hold, we
# stop BEFORE restoring, rather than overwriting existing content and only
# reporting it afterwards.
dst_tables_before=$(query "$dst_container" "$dst_db" \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'")
if [ "$dst_tables_before" = "0" ]; then
    ok "target empty before restore (public tables): 0"
else
    echo "restore-test: the TARGET database is not empty ($dst_tables_before public table(s))." >&2
    echo "restore-test: restore ABANDONED, no data was overwritten." >&2
    exit 1
fi

echo "restore-test: restoring (gunzip then psql)"
restore_started=$(date -u +%s)
gunzip -c "$archive" > "$workdir/restore.sql"
psql_in "$dst_container" "$dst_db" -q < "$workdir/restore.sql"
restore_ended=$(date -u +%s)
# Measured RTO: duration of the restore only (decompression + SQL replay),
# excluding server startup. Reported in docs/backup-restore.md.
restore_seconds=$((restore_ended - restore_started))

# --- Post-restore checks -----------------------------------------------------
echo "restore-test: post-restore checks"

for table in users business_connections chats messages notification_outbox schema_migrations; do
    present=$(query "$dst_container" "$dst_db" \
        "SELECT count(*) FROM information_schema.tables
         WHERE table_schema = 'public' AND table_name = '$table'")
    expect_eq "restored table: $table" "1" "$present"
done

dst_versions=$(query "$dst_container" "$dst_db" \
    "SELECT version FROM schema_migrations ORDER BY version")
if [ "$expected_versions" = "$dst_versions" ]; then
    ok "schema_migrations up to date: versions $(printf '%s' "$dst_versions" | tr '\n' ' ')"
else
    ko "schema_migrations: expected <$(printf '%s' "$expected_versions" | tr '\n' ' ')>, got <$(printf '%s' "$dst_versions" | tr '\n' ' ')>"
fi

expect_eq "users count" "$src_users" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM users')"
expect_eq "business_connections count" "$src_connections" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM business_connections')"
expect_eq "chats count" "$src_chats" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM chats')"
expect_eq "messages count" "$src_messages" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM messages')"
expect_eq "notification_outbox count" "$src_outbox" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM notification_outbox')"

# Content integrity, not just cardinality: a truncated dump can restore the
# right number of rows with empty columns.
expect_eq "restored message canary content" "$src_canary" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT text_content FROM messages WHERE chat_id = -100999001 AND message_id = 4242')"
expect_eq "restored chat title" "RESTORE-TEST chat" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT title FROM chats WHERE chat_id = -100999001')"
expect_eq "restored outbox payload" "RESTORE-TEST canary payload" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT payload_text FROM notification_outbox WHERE message_id = 4242')"

# FORCE RLS is a schema property: if the dump lost it, the restored database
# would be open to all tenants without any count changing.
# The relnamespace filter is required: pg_class covers ALL schemas, so a
# homonym elsewhere would skew the count one way or the other.
rls_forced=$(query "$dst_container" "$dst_db" \
    "SELECT count(*) FROM pg_class
     WHERE relnamespace = 'public'::regnamespace
       AND relname IN ('messages', 'notification_outbox', 'chats')
       AND relrowsecurity AND relforcerowsecurity")
expect_eq "FORCE ROW LEVEL SECURITY restored (3 tables)" "3" "$rls_forced"

echo
echo "restore-test: measured RTO (restore only): ${restore_seconds}s"
if [ "$failures" -eq 0 ]; then
    echo "restore-test: SUCCESS -- the backup is restorable and verified."
    exit 0
fi
echo "restore-test: FAILURE -- $failures check(s) failing." >&2
exit 1
