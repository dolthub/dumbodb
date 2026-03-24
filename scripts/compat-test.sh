#!/usr/bin/env bash
# Run FerretDB compat tests: compare Dongo (target) against real MongoDB (compat).
#
# Requires MongoDB-secure to be running on MONGO_PORT (default 47017).
# Starts Dongo on DONGO_PORT (default 27017), runs compat suite, stops Dongo.
#
# Usage:
#   compat-test.sh [results-file]
#
# Environment:
#   DONGO_PORT    Port dongo listens on (default: 27017)
#   MONGO_PORT    Port of compat MongoDB (default: 47017)
#   MONGO_USER    MongoDB username (default: username)
#   MONGO_PASS    MongoDB password (default: password)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DONGO_BINARY="$REPO_ROOT/.runtime/bin/dongo"
FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"
DONGO_HOST="127.0.0.1"
DONGO_PORT="${DONGO_PORT:-27017}"
DONGO_ADDR="$DONGO_HOST:$DONGO_PORT"
DONGO_URL="mongodb://$DONGO_ADDR/"
DONGO_DATA_DIR="$REPO_ROOT/.runtime/dongo-compat-data"
DONGO_LOG="$REPO_ROOT/.runtime/dongo-compat.log"

MONGO_HOST="127.0.0.1"
MONGO_PORT="${MONGO_PORT:-47017}"
MONGO_USER="${MONGO_USER:-username}"
MONGO_PASS="${MONGO_PASS:-password}"
COMPAT_URL="mongodb://${MONGO_USER}:${MONGO_PASS}@${MONGO_HOST}:${MONGO_PORT}/?replicaSet=rs0"

RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-compat.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"
mkdir -p "$DONGO_DATA_DIR"
mkdir -p "$(dirname "$DONGO_LOG")"

echo "=== Dongo FerretDB Compat Suite ===" | tee "$RESULTS_FILE"
echo "Date:   $(date -u)" | tee -a "$RESULTS_FILE"
echo "Target: $DONGO_URL (dongo)" | tee -a "$RESULTS_FILE"
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

# Start Dongo.
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
echo "--- Running FerretDB compat tests ---" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

cd "$FERRETDB_INTEGRATION"
set +e
go test -count=1 -timeout=0 \
    -tags=ferretdb_dev \
    -race=false \
    -target-backend=ferretdb \
    -target-url="$DONGO_URL" \
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
