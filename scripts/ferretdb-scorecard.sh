#!/usr/bin/env bash
# Run FerretDB integration tests against a running DumboDB instance.
# Usage: ferretdb-scorecard.sh [results-file]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DUMBODB_BINARY="$REPO_ROOT/.runtime/bin/dumbodb"
FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"
DUMBODB_HOST="127.0.0.1"
DUMBODB_PORT="27017"
DUMBODB_ADDR="$DUMBODB_HOST:$DUMBODB_PORT"
# MongoDB URI requires a trailing slash before query parameters.
DUMBODB_URL="mongodb://$DUMBODB_ADDR/"
DUMBODB_DATA_DIR="$REPO_ROOT/.runtime/dumbodb-data"
DUMBODB_LOG="$REPO_ROOT/.runtime/dumbodb.log"
RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-scorecard.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"
# Always start with a clean data directory so schema changes don't cause
# migration failures from stale data left by previous runs.
rm -rf "$DUMBODB_DATA_DIR"
mkdir -p "$DUMBODB_DATA_DIR"
mkdir -p "$(dirname "$DUMBODB_LOG")"

echo "=== DumboDB FerretDB Compatibility Scorecard ===" | tee "$RESULTS_FILE"
echo "Date: $(date -u)" | tee -a "$RESULTS_FILE"
echo "DumboDB: $DUMBODB_BINARY" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Start DumboDB server in background.
"$DUMBODB_BINARY" --addr "$DUMBODB_ADDR" --data-dir "$DUMBODB_DATA_DIR" >"$DUMBODB_LOG" 2>&1 &
DUMBODB_PID=$!

cleanup() {
    echo ""
    echo "Stopping DumboDB (PID: $DUMBODB_PID)..."
    kill "$DUMBODB_PID" 2>/dev/null || true
    wait "$DUMBODB_PID" 2>/dev/null || true
    echo "DumboDB stopped."
}
trap cleanup EXIT

# Wait up to 30 seconds for DumboDB to accept connections.
echo "Waiting for DumboDB on $DUMBODB_ADDR..." | tee -a "$RESULTS_FILE"
READY=0
for i in $(seq 1 30); do
    if nc -z "$DUMBODB_HOST" "$DUMBODB_PORT" 2>/dev/null; then
        echo "DumboDB ready after ${i}s" | tee -a "$RESULTS_FILE"
        READY=1
        break
    fi
    if ! kill -0 "$DUMBODB_PID" 2>/dev/null; then
        echo "ERROR: DumboDB exited prematurely. Server log:" | tee -a "$RESULTS_FILE"
        cat "$DUMBODB_LOG" | tee -a "$RESULTS_FILE"
        exit 1
    fi
    sleep 1
done

if [ "$READY" -eq 0 ]; then
    echo "ERROR: DumboDB did not start within 30s. Server log:" | tee -a "$RESULTS_FILE"
    cat "$DUMBODB_LOG" | tee -a "$RESULTS_FILE"
    exit 1
fi

# Create the standard FerretDB integration test user so auth-dependent tests
# (e.g. TestHelloOpQuerySASLSupportedMechs) can run against this server.
echo "Creating scorecard user..." | tee -a "$RESULTS_FILE"
cd "$REPO_ROOT"
if ! go run scripts/create_scorecard_user.go "$DUMBODB_URL" 2>&1 | tee -a "$RESULTS_FILE"; then
    echo "ERROR: Failed to create scorecard user" | tee -a "$RESULTS_FILE"
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
    -target-url="$DUMBODB_URL" \
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
