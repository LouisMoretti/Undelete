#!/bin/sh
# Fuzz only the packages touched by the current branch.
#
# Resolves every changed .go file under bot/ to its package directory, then
# runs each Fuzz* target declared there for FUZZTIME seconds. Exits 0 with a
# note when nothing fuzzable was touched (the seed corpus still runs as
# ordinary unit tests in the lint-unit job, so coverage never drops to zero).
#
# Needs the base ref locally (actions/checkout with fetch-depth: 0).
# Usage: sh scripts/fuzz-touched.sh [base-ref] [fuzztime-seconds]
#   sh scripts/fuzz-touched.sh origin/dev 60        # PR against dev
#   sh scripts/fuzz-touched.sh HEAD~1 60            # direct push
#   sh scripts/fuzz-touched.sh dev 5                # quick local check
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BASE=${1:-origin/dev}
FUZZTIME=${2:-60}

if ! git -C "$ROOT" rev-parse --verify --quiet "$BASE" >/dev/null; then
    echo "fuzz-touched: base ref $BASE unknown locally, nothing to fuzz" >&2
    exit 0
fi

CHANGED=$(git -C "$ROOT" diff --name-only "${BASE}...HEAD" -- bot | grep '\.go$' || true)
if [ -z "$CHANGED" ]; then
    echo "fuzz-touched: no Go changes under bot/, nothing to fuzz"
    exit 0
fi

echo "$CHANGED" | while IFS= read -r f; do dirname "$f"; done | sort -u | while IFS= read -r d; do
    rel=${d#bot/}
    targets=$(grep -h '^func Fuzz' "$ROOT/$d"/*_test.go 2>/dev/null | sed 's/^func \(Fuzz[A-Za-z0-9_]*\).*/\1/' | sort -u || true)
    if [ -z "$targets" ]; then
        echo "fuzz-touched: $d has no Fuzz targets, skipping"
        continue
    fi
    echo "$targets" | while IFS= read -r target; do
        echo "fuzz-touched: $target in ./$rel for ${FUZZTIME}s"
        (cd "$ROOT/bot" && go test -run=NONE -fuzz="$target" -fuzztime="${FUZZTIME}s" "./$rel")
    done
done
