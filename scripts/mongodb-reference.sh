#!/usr/bin/env bash
# Run FerretDB integration tests against real MongoDB as a reference baseline.
#
# This establishes a "gold standard" scorecard showing which tests pass against
# MongoDB itself, so you can distinguish:
#   - docudolt regressions (passes here, fails in ferretdb-scorecard)
#   - expected failures (fails here too, not a docudolt-specific problem)
#
# Requires MongoDB running on MONGO_HOST:MONGO_PORT (no auth).
#
# Usage:
#   mongodb-reference.sh [results-file]
#
# Environment:
#   MONGO_HOST    MongoDB hostname (default: 127.0.0.1)
#   MONGO_PORT    MongoDB port, no-auth instance (default: 37017)
#
# Start MongoDB first:
#   cd ferretdb && docker compose up -d mongodb
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"

MONGO_HOST="${MONGO_HOST:-127.0.0.1}"
MONGO_PORT="${MONGO_PORT:-37017}"
MONGO_URL="mongodb://$MONGO_HOST:$MONGO_PORT/"

RESULTS_FILE="${1:-$REPO_ROOT/.runtime/mongodb-reference.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"

echo "=== MongoDB Reference Scorecard ===" | tee "$RESULTS_FILE"
echo "Date:   $(date -u)" | tee -a "$RESULTS_FILE"
echo "Target: $MONGO_URL (real MongoDB — no auth)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
echo "NOTE: Failures here are genuine MongoDB baseline results, not docudolt bugs." | tee -a "$RESULTS_FILE"
echo "      Compare with ferretdb-scorecard output to isolate docudolt regressions." | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Verify MongoDB is reachable.
if ! nc -z "$MONGO_HOST" "$MONGO_PORT" 2>/dev/null; then
    echo "ERROR: MongoDB not reachable at $MONGO_HOST:$MONGO_PORT" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"
    echo "Start it first:" | tee -a "$RESULTS_FILE"
    echo "  cd ferretdb && docker compose up -d mongodb" | tee -a "$RESULTS_FILE"
    exit 1
fi
echo "MongoDB reachable at $MONGO_HOST:$MONGO_PORT" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

echo "--- Running FerretDB integration tests against MongoDB ---" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

cd "$FERRETDB_INTEGRATION"
set +e
go test -count=1 -timeout=0 \
    -tags=ferretdb_dev \
    -race=false \
    -target-backend=mongodb \
    -target-url="$MONGO_URL" \
    ./... 2>&1 | tee -a "$RESULTS_FILE"
TEST_EXIT=${PIPESTATUS[0]}
set -e

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
PASS_COUNT=$(grep -c "^ok " "$RESULTS_FILE" || true)
FAIL_COUNT=$(grep -c "^FAIL\s" "$RESULTS_FILE" || true)
echo "Packages passed : $PASS_COUNT" | tee -a "$RESULTS_FILE"
echo "Packages failed : $FAIL_COUNT" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
echo "Results written to: $RESULTS_FILE"

exit "$TEST_EXIT"
