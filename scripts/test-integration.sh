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
        # Les tests outbox vivent hors du tag `integration` et sont gates par
        # leurs propres variables : sans ce câblage ils s’ignoreraient (t.Skip)
        # alors que la base migrée par la suite ci-dessus est juste là. Le DSN
        # runtime alimente le rôle applicatif, le DSN admin la preuve de
        # rollback atomique (trigger temporaire, donc propriétaire requis).
        # Un réglage fourni par l’appelant reste prioritaire.
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

# Les deux conditions visent 127.0.0.1 et non le socket Unix : pendant
# docker-entrypoint-initdb.d, l'image officielle démarre un serveur temporaire
# accessible uniquement par socket. Un pg_isready sur socket réussirait donc
# AVANT la fin des scripts d'init et avant l'écoute TCP, et le port publié
# refuserait ensuite la connexion par intermittence. La requête sur pg_roles
# confirme en plus que db/init/01-app-role.sh est allé au bout.
# PGPASSWORD est indispensable : le pg_hba.conf généré par l'image impose
# scram-sha-256 pour les connexions host, y compris depuis le conteneur.
attempt=0
until docker exec "$container" pg_isready -h 127.0.0.1 -U postgres -d undelete_integration >/dev/null 2>&1 &&
    docker exec --env PGPASSWORD="$admin_password" "$container" \
        psql -h 127.0.0.1 -U postgres -d undelete_integration -tAc \
        "SELECT 1 FROM pg_roles WHERE rolname = 'undelete_app'" 2>/dev/null | grep -qx 1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 60 ]; then
        docker logs "$container" >&2 || true
        echo "PostgreSQL 16 or the undelete_app role did not become ready" >&2
        exit 1
    fi
    sleep 1
done

port=$(docker port "$container" 5432/tcp | awk -F: 'NR == 1 { print $NF }')
export POSTGRES_INTEGRATION_ADMIN_DSN="postgres://postgres:${admin_password}@127.0.0.1:$port/undelete_integration?sslmode=disable"
export POSTGRES_INTEGRATION_RUNTIME_DSN="postgres://undelete_app:${runtime_password}@127.0.0.1:$port/undelete_integration?sslmode=disable"
export POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA
run_tests
