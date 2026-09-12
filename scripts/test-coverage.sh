#!/bin/sh
# Combined unit + integration statement coverage with a floor gate.
#
# Runs the unit suite, then the PostgreSQL 16 integration suites in the same
# order as test-integration.sh (sequential: the integration TRUNCATE would
# wipe outbox fixtures under a parallel run), merges the three profiles and
# fails when the combined total drops below COVERAGE_FLOOR (default 75).
#
# Always boots its own disposable container: existing containers, networks,
# images and volumes are never touched.
# Usage: sh scripts/test-coverage.sh   (COVERAGE_FLOOR=80 to override)
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=lib-integration-pg.sh
. "$ROOT/scripts/lib-integration-pg.sh"

FLOOR=${COVERAGE_FLOOR:-75}

start_disposable_postgres coverage

COV_DIR=$(mktemp -d)
trap 'cleanup_disposable_postgres; rm -rf "$COV_DIR"' EXIT HUP INT TERM

(
    cd "$ROOT/bot"
    # Unit suite: no DSNs here, so tag-gated suites stay out and the
    # env-gated outbox tests skip themselves, exactly like the CI lint job.
    go test -coverprofile="$COV_DIR/unit.out" -covermode=atomic -count=1 ./...
    # Integration package (migrates the database, hence first against the DB).
    go test -tags=integration -coverprofile="$COV_DIR/integration.out" -covermode=atomic -coverpkg=./... -count=1 ./integration
    # Outbox PostgreSQL tests, wired like test-integration.sh.
    OUTBOX_TEST_DATABASE_URL="$POSTGRES_INTEGRATION_RUNTIME_DSN" \
        OUTBOX_TEST_MIGRATION_DATABASE_URL="$POSTGRES_INTEGRATION_ADMIN_DSN" \
        go test -coverprofile="$COV_DIR/outbox.out" -covermode=atomic -coverpkg=./... -count=1 ./internal/outbox
)

# Merge: one line per block, counts summed across profiles.
awk '/^mode:/{next} {k=$1; if(!(k in S)){O[++n]=k; S[k]=$2} C[k]+=$3} END{print "mode: atomic"; for(i=1;i<=n;i++){k=O[i]; print k, S[k], C[k]}}' \
    "$COV_DIR"/unit.out "$COV_DIR"/integration.out "$COV_DIR"/outbox.out > "$COV_DIR/all.out"

RAW=$(awk '
/^mode:/{next}
{
    f=$1; sub(/:[0-9].*$/, "", f); sub(/^.*\/bot\//, "", f); sub(/\/[^\/]*$/, "", f)
    if (f == "") f="(root-cmd)"
    T[f]+=$2; if ($3 > 0) H[f]+=$2; GT+=$2; if ($3 > 0) GH+=$2
}
END{
    for (p in T) {
        pct = (T[p] > 0) ? 100*H[p]/T[p] : 0
        printf "%05d %6.1f%%  %s\n", pct*10, pct, p
    }
    total = (GT > 0) ? 100*GH/GT : 0
    printf "TOTAL:%.1f\n", total
}' "$COV_DIR/all.out")

REPORT=$(echo "$RAW" | grep -v '^TOTAL:' | sort -rn | cut -d' ' -f2-)
TOTAL=$(echo "$RAW" | sed -n 's/^TOTAL://p')
REPORT="$REPORT
$(printf '%6.1f%%  TOTAL (floor %s%%)' "$TOTAL" "$FLOOR")"

echo "$REPORT"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
        echo "### Combined coverage (unit + integration)"
        echo '```'
        echo "$REPORT"
        echo '```'
    } >> "$GITHUB_STEP_SUMMARY"
fi

awk -v t="$TOTAL" -v f="$FLOOR" 'BEGIN {
    if (t + 0 < f + 0) {
        printf "combined coverage %s%% below floor %s%%\n", t, f
        exit 1
    }
}'
