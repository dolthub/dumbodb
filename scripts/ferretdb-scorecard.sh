#!/usr/bin/env bash
# Run FerretDB integration tests against a running Docudolt instance.
# Usage: ferretdb-scorecard.sh [results-file]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DOCUDOLT_BINARY="$REPO_ROOT/.runtime/bin/docudolt"
FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"
DOCUDOLT_HOST="127.0.0.1"
DOCUDOLT_PORT="27017"
DOCUDOLT_ADDR="$DOCUDOLT_HOST:$DOCUDOLT_PORT"
# MongoDB URI requires a trailing slash before query parameters.
DOCUDOLT_URL="mongodb://$DOCUDOLT_ADDR/"
DOCUDOLT_DATA_DIR="$REPO_ROOT/.runtime/docudolt-data"
DOCUDOLT_LOG="$REPO_ROOT/.runtime/docudolt.log"
RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-scorecard.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"
# Always start with a clean data directory so schema changes don't cause
# migration failures from stale data left by previous runs.
rm -rf "$DOCUDOLT_DATA_DIR"
mkdir -p "$DOCUDOLT_DATA_DIR"
mkdir -p "$(dirname "$DOCUDOLT_LOG")"

echo "=== Docudolt FerretDB Compatibility Scorecard ===" | tee "$RESULTS_FILE"
echo "Date: $(date -u)" | tee -a "$RESULTS_FILE"
echo "Docudolt: $DOCUDOLT_BINARY" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Start Docudolt server in background.
"$DOCUDOLT_BINARY" --addr "$DOCUDOLT_ADDR" --data-dir "$DOCUDOLT_DATA_DIR" >"$DOCUDOLT_LOG" 2>&1 &
DOCUDOLT_PID=$!

cleanup() {
    echo ""
    echo "Stopping Docudolt (PID: $DOCUDOLT_PID)..."
    kill "$DOCUDOLT_PID" 2>/dev/null || true
    wait "$DOCUDOLT_PID" 2>/dev/null || true
    echo "Docudolt stopped."
}
trap cleanup EXIT

# Wait up to 30 seconds for Docudolt to accept connections.
echo "Waiting for Docudolt on $DOCUDOLT_ADDR..." | tee -a "$RESULTS_FILE"
READY=0
for i in $(seq 1 30); do
    if nc -z "$DOCUDOLT_HOST" "$DOCUDOLT_PORT" 2>/dev/null; then
        echo "Docudolt ready after ${i}s" | tee -a "$RESULTS_FILE"
        READY=1
        break
    fi
    if ! kill -0 "$DOCUDOLT_PID" 2>/dev/null; then
        echo "ERROR: Docudolt exited prematurely. Server log:" | tee -a "$RESULTS_FILE"
        cat "$DOCUDOLT_LOG" | tee -a "$RESULTS_FILE"
        exit 1
    fi
    sleep 1
done

if [ "$READY" -eq 0 ]; then
    echo "ERROR: Docudolt did not start within 30s. Server log:" | tee -a "$RESULTS_FILE"
    cat "$DOCUDOLT_LOG" | tee -a "$RESULTS_FILE"
    exit 1
fi

# Create the standard FerretDB integration test user so auth-dependent tests
# (e.g. TestHelloOpQuerySASLSupportedMechs) can run against this server.
echo "Creating scorecard user..." | tee -a "$RESULTS_FILE"
cd "$REPO_ROOT"
if ! go run scripts/create_scorecard_user.go "$DOCUDOLT_URL" 2>&1 | tee -a "$RESULTS_FILE"; then
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
    -target-url="$DOCUDOLT_URL" \
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
