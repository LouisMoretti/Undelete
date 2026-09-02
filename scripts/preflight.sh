#!/bin/sh
# Deployment preflight: checks, WITHOUT MODIFYING ANYTHING, that the machine
# and configuration are ready before a `docker compose up`. No writes, no
# deletions, no destructive commands -- this script is read-only by design
# and can be re-run as many times as desired.
#
# Usage:
#   sh scripts/preflight.sh
#
# Output: one line per check, prefixed [ OK ] / [FAIL] / [SKIP].
# Exit code 0 if no FAIL, 1 otherwise. A [SKIP] (missing tool, database
# unreachable from the host) is never blocking: it signals a check that
# was NOT RUN, to be redone from a place that allows it.
#
# NOTE: the Telegram token is never displayed. Any external output
# (API response) passes through mask_token() before being printed.
set -eu

# Free disk space threshold, in gigabytes (overridable).
PREFLIGHT_MIN_DISK_GB="${PREFLIGHT_MIN_DISK_GB:-2}"

# Repository root: the script can be invoked from any directory.
repo_root="$(cd "$(dirname "$0")/.." && pwd)"

failure_count=0
skip_count=0

ok() { echo "[ OK ] $1"; }
fail() { echo "[FAIL] $1"; failure_count=$((failure_count + 1)); }
skip() { echo "[SKIP] $1"; skip_count=$((skip_count + 1)); }

# Replaces the Telegram token with a masked form in arbitrary text.
# Called on EVERYTHING that comes from the API: a curl error message or a
# Telegram response may echo back the called URL, token included.
mask_token() {
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
        sed "s|${TELEGRAM_BOT_TOKEN}|<TELEGRAM_BOT_TOKEN masked>|g"
    else
        cat
    fi
}

echo "=== undelete preflight ==="
echo "repository: ${repo_root}"
echo

# --- 1. Loading .env -------------------------------------------------------
# The .env file is NOT a shell script (docker compose reads it as a list of
# key=value lines). It is parsed line by line rather than sourced: a `.` on
# a secrets file would execute whatever it contains.
#
# Precedence identical to docker compose: a variable already present in the
# environment wins over the value in the file.
env_file="${repo_root}/.env"
if [ -f "$env_file" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
            ''|'#'*) continue ;;
            *=*) ;;
            *) continue ;;
        esac
        key="${line%%=*}"
        value="${line#*=}"
        key="${key#"${key%%[![:space:]]*}"}"
        key="${key%"${key##*[![:space:]]}"}"
        case "$key" in
            ''|*[!A-Za-z0-9_]*) continue ;;
        esac
        # Removes an optional pair of surrounding quotes.
        case "$value" in
            \"*\") value="${value#\"}"; value="${value%\"}" ;;
            \'*\') value="${value#\'}"; value="${value%\'}" ;;
        esac
        # eval is required to read the current value of a dynamic name in pure
        # POSIX; the name is validated above ([A-Za-z0-9_] only).
        eval "current_value=\${${key}:-}"
        if [ -z "$current_value" ]; then
            export "${key}=${value}"
        fi
    done < "$env_file"
    ok ".env present and loaded (${env_file})"

    # Permissions: .env contains the bot token and the Postgres
    # passwords. Readable by group or by all = silent leak.
    mode=""
    if command -v stat >/dev/null 2>&1; then
        mode="$(stat -c '%a' "$env_file" 2>/dev/null || stat -f '%Lp' "$env_file" 2>/dev/null || echo '')"
    fi
    if [ -z "$mode" ]; then
        skip ".env permissions: stat command unavailable, check manually (expected 600)"
    else
        case "$mode" in
            600|400) ok ".env permissions correct (${mode})" ;;
            *) fail ".env permissions too open (${mode}): expected 600 -- run \`chmod 600 .env\`" ;;
        esac
    fi
else
    skip ".env absent: variables are read from the current environment"
fi

# --- 2. Required variables -------------------------------------------------
# Modeled on .env.example. An empty variable counts as missing.
for var in POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB APP_DB_PASSWORD \
           MIGRATION_DATABASE_URL DATABASE_URL TELEGRAM_BOT_TOKEN; do
    eval "value=\${${var}:-}"
    if [ -n "$value" ]; then
        ok "variable ${var} set"
    else
        fail "variable ${var} missing or empty (see .env.example)"
    fi
done

# OWNER_TELEGRAM_USER_ID: optional from config.Load()'s point of view, but
# it is the Phase 1 mono-tenant guardrail. Empty = any Telegram account can
# connect the bot in Business mode. Blocking outside local development.
if [ -n "${OWNER_TELEGRAM_USER_ID:-}" ]; then
    case "$OWNER_TELEGRAM_USER_ID" in
        ''|*[!0-9]*) fail "OWNER_TELEGRAM_USER_ID must be an integer (non-numeric value)" ;;
        *) ok "OWNER_TELEGRAM_USER_ID set (mono-tenant guardrail active)" ;;
    esac
else
    fail "OWNER_TELEGRAM_USER_ID empty: no mono-tenant guardrail, any Business connection would be accepted (acceptable in local dev ONLY)"
fi

# BACKUP_RETENTION_DAYS has a default value in backup.sh (14): being absent
# is not an error, but a non-numeric value would break the `find -mtime`.
if [ -n "${BACKUP_RETENTION_DAYS:-}" ]; then
    case "$BACKUP_RETENTION_DAYS" in
        ''|*[!0-9]*) fail "BACKUP_RETENTION_DAYS must be an integer (non-numeric value)" ;;
        *) ok "BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS} days" ;;
    esac
else
    ok "BACKUP_RETENTION_DAYS not set: backup.sh will apply 14 days"
fi

# --- 3. App DSN != owner DSN -----------------------------------------------
# Same rule as config.Load(): identical DSNs => the bot would run with the
# owner role and FORCE ROW LEVEL SECURITY would become decorative.
if [ -n "${DATABASE_URL:-}" ] && [ -n "${MIGRATION_DATABASE_URL:-}" ]; then
    if [ "$DATABASE_URL" = "$MIGRATION_DATABASE_URL" ]; then
        fail "DATABASE_URL and MIGRATION_DATABASE_URL are identical: the bot will refuse to start (RLS would be decorative)"
    else
        ok "DATABASE_URL and MIGRATION_DATABASE_URL are distinct"
    fi
else
    skip "DSN comparison impossible: at least one of the two is missing"
fi

# --- 4. Free disk space ----------------------------------------------------
# Checked on the repository's filesystem: it is what hosts ./backups
# (gzip dumps) and, unless configured otherwise, the postgres_data volume.
free_kb="$(df -Pk "$repo_root" 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -z "$free_kb" ]; then
    skip "disk space: df returned nothing for ${repo_root}"
else
    free_gb=$((free_kb / 1024 / 1024))
    if [ "$free_gb" -ge "$PREFLIGHT_MIN_DISK_GB" ]; then
        ok "free disk space ${free_gb} GB (threshold ${PREFLIGHT_MIN_DISK_GB} GB)"
    else
        fail "free disk space ${free_gb} GB < threshold ${PREFLIGHT_MIN_DISK_GB} GB -- purge old dumps from ./backups, NEVER a Docker volume"
    fi
fi

# --- 5. Permissions of data directories ------------------------------------
# ./backups is a bind mount written by the backup service; ./media is written
# by the bot, which runs as uid 10001 with a read-only rootfs.
for directory in backups media; do
    path="${repo_root}/${directory}"
    if [ ! -d "$path" ]; then
        fail "directory ./${directory} missing: run \`mkdir -p ${path}\`"
    elif [ -w "$path" ]; then
        ok "directory ./${directory} present and writable"
    else
        fail "directory ./${directory} present but not writable"
    fi
done

# --- 6. PostgreSQL roles ---------------------------------------------------
# Checks that the owner role answers and that the app role exists without
# an RLS bypass privilege -- exactly what storage.NewPool re-checks at boot,
# but BEFORE deploying.
#
# From the host, the .env DSNs point to the `postgres` host of the Docker
# network and do not resolve: a connection failure is an explicit SKIP, not
# a FAIL. Re-run the check from the compose network then (see
# docs/runbook.md).
if ! command -v psql >/dev/null 2>&1; then
    skip "PostgreSQL roles: psql unavailable on this machine (see docs/runbook.md to re-run it via docker compose)"
elif [ -z "${MIGRATION_DATABASE_URL:-}" ]; then
    skip "PostgreSQL roles: MIGRATION_DATABASE_URL missing"
else
    # Short timeout: a preflight must never stay stuck on an unreachable host.
    export PGCONNECT_TIMEOUT=5
    psql_out=""
    if psql_out="$(psql "$MIGRATION_DATABASE_URL" -tAX -c 'SELECT current_user' 2>&1)"; then
        ok "owner role reachable (current_user=${psql_out})"

        attributes=""
        if attributes="$(psql "$MIGRATION_DATABASE_URL" -tAX \
            -c "SELECT rolsuper::text || ',' || rolbypassrls::text FROM pg_catalog.pg_roles WHERE rolname = 'undelete_app'" 2>&1)"; then
            case "$attributes" in
                "")
                    fail "role undelete_app missing: db/init/01-app-role.sh did not run (it only runs on the FIRST start of the postgres_data volume)"
                    ;;
                "false,false")
                    ok "role undelete_app present, NOSUPERUSER and NOBYPASSRLS"
                    ;;
                *)
                    fail "role undelete_app present but privileged (rolsuper,rolbypassrls = ${attributes}): RLS bypassable, the bot will refuse to start"
                    ;;
            esac
        else
            skip "undelete_app attributes unreadable: ${attributes}"
        fi
    else
        skip "database unreachable from this machine, roles NOT checked: ${psql_out}"
    fi
fi

# --- 7. Telegram token (getMe) ---------------------------------------------
# getMe is read-only and consumes no updates. The token travels in the URL:
# neither the URL nor the raw response is printed as-is.
#
# The URL is passed to curl via --config - (standard input) and NEVER as an
# argument: a process's command line is readable by any local user via ps and
# /proc/<pid>/cmdline, which would expose the token for the duration of the
# call -- outside the channel that mask_token() protects.
if [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then
    skip "Telegram token: TELEGRAM_BOT_TOKEN missing, getMe call not performed"
elif ! command -v curl >/dev/null 2>&1; then
    skip "Telegram token: curl unavailable on this machine"
else
    response=""
    if response="$(printf 'url = "https://api.telegram.org/bot%s/getMe"\n' \
        "$TELEGRAM_BOT_TOKEN" | curl -sS --max-time 10 --config - 2>&1)"; then
        case "$response" in
            *'"ok":true'*)
                # Username extraction without jq (absent from the alpine image).
                username="$(printf '%s' "$response" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p')"
                ok "Telegram token valid (getMe -> @${username:-unknown})"
                ;;
            *)
                detail="$(printf '%s' "$response" | mask_token | head -c 200)"
                fail "Telegram token rejected by the API: ${detail}"
                ;;
        esac
    else
        detail="$(printf '%s' "$response" | mask_token | head -c 200)"
        skip "getMe call impossible (network?): ${detail}"
    fi
fi

# --- Summary ---------------------------------------------------------------
echo
echo "=== Summary: ${failure_count} failure(s), ${skip_count} check(s) not run ==="
if [ "$failure_count" -gt 0 ]; then
    echo "Preflight FAILED: fix the points above before any deployment."
    exit 1
fi
if [ "$skip_count" -gt 0 ]; then
    echo "Preflight OK, but some checks could not be run (see [SKIP])."
fi
echo "Preflight OK."
