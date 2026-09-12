#!/bin/sh
# Run the PostgreSQL 16 integration suite against either caller-provided DSNs
# or an isolated, disposable Docker container. This script never touches
# existing containers, networks, images, or volumes.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=lib-integration-pg.sh
. "$ROOT/scripts/lib-integration-pg.sh"

run_tests() {
    (
        cd "$ROOT/bot"
        go test -tags=integration -count=1 -v ./integration
        # The outbox tests live outside the `integration` tag and are gated by
        # their own variables: without this wiring they would skip themselves
        # (t.Skip) even though the database migrated by the suite above is
        # right there. The runtime DSN feeds the app role, the admin DSN proves
        # atomic rollback (temporary trigger, hence owner required).
        # A setting supplied by the caller takes precedence.
        OUTBOX_TEST_DATABASE_URL="${OUTBOX_TEST_DATABASE_URL:-$POSTGRES_INTEGRATION_RUNTIME_DSN}" \
            OUTBOX_TEST_MIGRATION_DATABASE_URL="${OUTBOX_TEST_MIGRATION_DATABASE_URL:-$POSTGRES_INTEGRATION_ADMIN_DSN}" \
            go test -count=1 -v ./internal/outbox
    )
}

if [ -n "${POSTGRES_INTEGRATION_ADMIN_DSN:-}" ] || [ -n "${POSTGRES_INTEGRATION_RUNTIME_DSN:-}" ]; then
    : "${POSTGRES_INTEGRATION_ADMIN_DSN:?both integration DSNs must be set}"
    : "${POSTGRES_INTEGRATION_RUNTIME_DSN:?both integration DSNs must be set}"
    if [ "${POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE:-}" != "I_UNDERSTAND_THIS_WILL_DELETE_DATA" ]; then
        cat >&2 <<'EOF'
Refusing caller-provided PostgreSQL DSNs: this suite truncates data.
Use a dedicated database named exactly "undelete_integration" and explicitly set:
  POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA
The Go suite also verifies current_database() on the server before migrations or TRUNCATE.
EOF
        exit 1
    fi
    run_tests
    exit 0
fi

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
PostgreSQL integration tests were not run: the Docker daemon is unavailable.
Either grant access to the Docker socket or start a local PostgreSQL 16 instance,
create the restricted role with db/init/01-app-role.sh, and set both:
  POSTGRES_INTEGRATION_ADMIN_DSN
  POSTGRES_INTEGRATION_RUNTIME_DSN
No existing Docker container, network, image, or volume was changed.
EOF
    exit 1
fi

# Disposable container path (caller-DSN mode returned above): the bootstrap
# lives in lib-integration-pg.sh so the coverage script reuses it verbatim.
start_disposable_postgres integration
run_tests
