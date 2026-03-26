#!/usr/bin/env bash
# Run FerretDB integration tests against a running Dongo instance.
# Usage: ferretdb-scorecard.sh [results-file]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DONGO_BINARY="$REPO_ROOT/.runtime/bin/dongo"
FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"
DONGO_HOST="127.0.0.1"
DONGO_PORT="27017"
DONGO_ADDR="$DONGO_HOST:$DONGO_PORT"
# MongoDB URI requires a trailing slash before query parameters.
DONGO_URL="mongodb://$DONGO_ADDR/"
DONGO_DATA_DIR="$REPO_ROOT/.runtime/dongo-data"
DONGO_LOG="$REPO_ROOT/.runtime/dongo.log"
RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-scorecard.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"
# Always start with a clean data directory so schema changes don't cause
# migration failures from stale data left by previous runs.
rm -rf "$DONGO_DATA_DIR"
mkdir -p "$DONGO_DATA_DIR"
mkdir -p "$(dirname "$DONGO_LOG")"

echo "=== Dongo FerretDB Compatibility Scorecard ===" | tee "$RESULTS_FILE"
echo "Date: $(date -u)" | tee -a "$RESULTS_FILE"
echo "Dongo: $DONGO_BINARY" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Start Dongo server in background.
"$DONGO_BINARY" --addr "$DONGO_ADDR" --data-dir "$DONGO_DATA_DIR" >"$DONGO_LOG" 2>&1 &
DONGO_PID=$!

cleanup() {
    echo ""
    echo "Stopping Dongo (PID: $DONGO_PID)..."
    kill "$DONGO_PID" 2>/dev/null || true
    wait "$DONGO_PID" 2>/dev/null || true
    echo "Dongo stopped."
}
trap cleanup EXIT

# Wait up to 30 seconds for Dongo to accept connections.
echo "Waiting for Dongo on $DONGO_ADDR..." | tee -a "$RESULTS_FILE"
READY=0
for i in $(seq 1 30); do
    if nc -z "$DONGO_HOST" "$DONGO_PORT" 2>/dev/null; then
        echo "Dongo ready after ${i}s" | tee -a "$RESULTS_FILE"
        READY=1
        break
    fi
    if ! kill -0 "$DONGO_PID" 2>/dev/null; then
        echo "ERROR: Dongo exited prematurely. Server log:" | tee -a "$RESULTS_FILE"
        cat "$DONGO_LOG" | tee -a "$RESULTS_FILE"
        exit 1
    fi
    sleep 1
done

if [ "$READY" -eq 0 ]; then
    echo "ERROR: Dongo did not start within 30s. Server log:" | tee -a "$RESULTS_FILE"
    cat "$DONGO_LOG" | tee -a "$RESULTS_FILE"
    exit 1
fi

echo "" | tee -a "$RESULTS_FILE"
echo "--- Running FerretDB integration tests ---" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Build -skip regexp from skiplist so known-failing tests are skipped at
# runtime (they appear as --- SKIP: lines, not --- FAIL: lines).
SKIPLIST_FILE="$REPO_ROOT/scripts/ferretdb-scorecard-skiplist.txt"
SKIP_PATTERN=$(grep -v '^\s*#' "$SKIPLIST_FILE" | grep -v '^\s*$' | paste -sd'|')

# Run integration tests. -race=false for speed; ferretdb_dev tag required.
# -v is required so that "--- PASS:" lines appear in output for pass-count reporting.
cd "$FERRETDB_INTEGRATION"
set +e
go test -count=1 -timeout=0 \
    -v \
    -tags=ferretdb_dev \
    -race=false \
    -skip "$SKIP_PATTERN" \
    -target-backend=mongodb \
    -target-url="$DONGO_URL" \
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
