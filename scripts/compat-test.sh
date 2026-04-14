#!/usr/bin/env bash
# Run FerretDB compat tests: compare DumboDB (target) against real MongoDB (compat).
#
# Requires MongoDB-secure to be running on MONGO_PORT (default 47017).
# Starts DumboDB on DUMBODB_PORT (default 27017), runs compat suite, stops DumboDB.
#
# Usage:
#   compat-test.sh [results-file]
#
# Environment:
#   DUMBODB_PORT    Port dumbodb listens on (default: 27017)
#   MONGO_PORT    Port of compat MongoDB (default: 47017)
#   MONGO_USER    MongoDB username (default: username)
#   MONGO_PASS    MongoDB password (default: password)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DUMBODB_BINARY="$REPO_ROOT/.runtime/bin/dumbodb"
FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"
DUMBODB_HOST="127.0.0.1"
DUMBODB_PORT="${DUMBODB_PORT:-27017}"
DUMBODB_ADDR="$DUMBODB_HOST:$DUMBODB_PORT"
DUMBODB_URL="mongodb://$DUMBODB_ADDR/"
DUMBODB_DATA_DIR="$REPO_ROOT/.runtime/dumbodb-compat-data"
DUMBODB_LOG="$REPO_ROOT/.runtime/dumbodb-compat.log"

MONGO_HOST="127.0.0.1"
MONGO_PORT="${MONGO_PORT:-47017}"
MONGO_USER="${MONGO_USER:-username}"
MONGO_PASS="${MONGO_PASS:-password}"
COMPAT_URL="mongodb://${MONGO_USER}:${MONGO_PASS}@${MONGO_HOST}:${MONGO_PORT}/?replicaSet=rs0"

RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-compat.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"
mkdir -p "$DUMBODB_DATA_DIR"
mkdir -p "$(dirname "$DUMBODB_LOG")"

echo "=== DumboDB FerretDB Compat Suite ===" | tee "$RESULTS_FILE"
echo "Date:   $(date -u)" | tee -a "$RESULTS_FILE"
echo "Target: $DUMBODB_URL (dumbodb)" | tee -a "$RESULTS_FILE"
echo "Compat: $COMPAT_URL" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Verify compat MongoDB is reachable before we start.
if ! nc -z "$MONGO_HOST" "$MONGO_PORT" 2>/dev/null; then
    echo "ERROR: MongoDB not reachable at $MONGO_HOST:$MONGO_PORT" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"
    echo "Start it first, e.g.:" | tee -a "$RESULTS_FILE"
    echo "  cd ferretdb && docker compose up -d mongodb-secure" | tee -a "$RESULTS_FILE"
    exit 1
fi

# Start DumboDB.
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

echo "" | tee -a "$RESULTS_FILE"
echo "--- Running FerretDB compat tests ---" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

cd "$FERRETDB_INTEGRATION"
set +e
go test -count=1 -timeout=0 \
    -tags=ferretdb_dev \
    -race=false \
    -target-backend=ferretdb \
    -target-url="$DUMBODB_URL" \
    -compat-url="$COMPAT_URL" \
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
