#!/bin/sh
# Run the PostgreSQL 16 integration suite against either caller-provided DSNs
# or an isolated, disposable Docker container. This script never touches
# existing containers, networks, images, or volumes.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

run_tests() {
    (
        cd "$ROOT/bot"
        go test -tags=integration -count=1 -v ./integration
    )
}

if [ -n "${POSTGRES_INTEGRATION_ADMIN_DSN:-}" ] || [ -n "${POSTGRES_INTEGRATION_RUNTIME_DSN:-}" ]; then
    : "${POSTGRES_INTEGRATION_ADMIN_DSN:?both integration DSNs must be set}"
    : "${POSTGRES_INTEGRATION_RUNTIME_DSN:?both integration DSNs must be set}"
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

suffix="$$-$(date +%s)"
container="undelete-pg-integration-$suffix"
admin_password="integration-admin-only"
runtime_password="integration-runtime-only"

cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

docker run --detach --rm \
    --name "$container" \
    --publish 127.0.0.1::5432 \
    --env POSTGRES_USER=postgres \
    --env POSTGRES_PASSWORD="$admin_password" \
    --env POSTGRES_DB=undelete_integration \
    --env APP_DB_USER=undelete_app \
    --env APP_DB_PASSWORD="$runtime_password" \
    --volume "$ROOT/db/init:/docker-entrypoint-initdb.d:ro" \
    postgres:16-alpine >/dev/null

attempt=0
until docker exec "$container" pg_isready -U postgres -d undelete_integration >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 60 ]; then
        docker logs "$container" >&2 || true
        echo "PostgreSQL 16 did not become ready" >&2
        exit 1
    fi
    sleep 1
done

port=$(docker port "$container" 5432/tcp | awk -F: 'NR == 1 { print $NF }')
export POSTGRES_INTEGRATION_ADMIN_DSN="postgres://postgres:$admin_password@127.0.0.1:$port/undelete_integration?sslmode=disable"
export POSTGRES_INTEGRATION_RUNTIME_DSN="postgres://undelete_app:$runtime_password@127.0.0.1:$port/undelete_integration?sslmode=disable"
run_tests
