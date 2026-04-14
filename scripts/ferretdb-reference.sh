#!/usr/bin/env bash
# Run FerretDB integration tests against a FerretDB server as a reference baseline.
#
# This shows how the same test suite performs against FerretDB itself, so you can:
#   - See which failures are FerretDB-level (not dumbodb-specific)
#   - Compare dumbodb's scorecard against the FerretDB baseline
#
# Usage:
#   ferretdb-reference.sh [results-file]
#
# Environment:
#   FERRETDB_HOST         FerretDB hostname (default: 127.0.0.1)
#   FERRETDB_PORT         FerretDB port (default: 27018)
#   POSTGRESQL_URL        PostgreSQL URL for in-process FerretDB mode (optional).
#                         If set, FerretDB runs in-process (no FERRETDB_PORT needed).
#                         Requires a PostgreSQL instance with the DocumentDB extension.
#
# External server mode (default):
#   Start FerretDB separately, then run this script.
#   Example:
#     # Build FerretDB from the submodule:
#     cd ferretdb && go build -o /tmp/ferretdb ./cmd/ferretdb/
#     # Start PostgreSQL:
#     cd ferretdb && docker compose up -d postgres
#     # Run FerretDB:
#     /tmp/ferretdb --listen-addr=127.0.0.1:27018 \
#       --postgresql-url='postgres://pg-user:pg-pass@127.0.0.1:5432/postgres' &
#     # Then run this script.
#
# In-process mode (POSTGRESQL_URL set):
#   cd ferretdb && docker compose up -d postgres
#   POSTGRESQL_URL='postgres://pg-user:pg-pass@127.0.0.1:5432/postgres' \
#     ./scripts/ferretdb-reference.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FERRETDB_INTEGRATION="$REPO_ROOT/ferretdb/integration"

FERRETDB_HOST="${FERRETDB_HOST:-127.0.0.1}"
FERRETDB_PORT="${FERRETDB_PORT:-27018}"
FERRETDB_URL="mongodb://$FERRETDB_HOST:$FERRETDB_PORT/"
POSTGRESQL_URL="${POSTGRESQL_URL:-}"

RESULTS_FILE="${1:-$REPO_ROOT/.runtime/ferretdb-reference.txt}"

mkdir -p "$(dirname "$RESULTS_FILE")"

echo "=== FerretDB Reference Scorecard ===" | tee "$RESULTS_FILE"
echo "Date: $(date -u)" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
echo "NOTE: Failures here reflect FerretDB's own baseline, not dumbodb-specific bugs." | tee -a "$RESULTS_FILE"
echo "      Compare with ferretdb-scorecard output to isolate dumbodb regressions." | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

if [ -n "$POSTGRESQL_URL" ]; then
    # In-process FerretDB mode: no target-url, FerretDB starts inside the test binary.
    echo "Mode:   in-process FerretDB (postgresql-url provided)" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"
    echo "--- Running FerretDB integration tests with in-process FerretDB ---" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"

    cd "$FERRETDB_INTEGRATION"
    set +e
    go test -count=1 -timeout=0 \
        -tags=ferretdb_dev \
        -race=false \
        -target-backend=ferretdb \
        -postgresql-url="$POSTGRESQL_URL" \
        ./... 2>&1 | tee -a "$RESULTS_FILE"
    TEST_EXIT=${PIPESTATUS[0]}
    set -e
else
    # External FerretDB server mode.
    echo "Mode:   external FerretDB server at $FERRETDB_URL" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"

    if ! nc -z "$FERRETDB_HOST" "$FERRETDB_PORT" 2>/dev/null; then
        echo "ERROR: FerretDB not reachable at $FERRETDB_HOST:$FERRETDB_PORT" | tee -a "$RESULTS_FILE"
        echo "" | tee -a "$RESULTS_FILE"
        echo "Start FerretDB first (external server mode), or provide POSTGRESQL_URL" | tee -a "$RESULTS_FILE"
        echo "to use in-process mode. See script header for details." | tee -a "$RESULTS_FILE"
        exit 1
    fi
    echo "FerretDB reachable at $FERRETDB_HOST:$FERRETDB_PORT" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"

    echo "--- Running FerretDB integration tests against external FerretDB ---" | tee -a "$RESULTS_FILE"
    echo "" | tee -a "$RESULTS_FILE"

    cd "$FERRETDB_INTEGRATION"
    set +e
    go test -count=1 -timeout=0 \
        -tags=ferretdb_dev \
        -race=false \
        -target-backend=ferretdb \
        -target-url="$FERRETDB_URL" \
        ./... 2>&1 | tee -a "$RESULTS_FILE"
    TEST_EXIT=${PIPESTATUS[0]}
    set -e
fi

echo "" | tee -a "$RESULTS_FILE"
echo "=== Summary ===" | tee -a "$RESULTS_FILE"
PASS_COUNT=$(grep -c "^ok " "$RESULTS_FILE" || true)
FAIL_COUNT=$(grep -c "^FAIL\s" "$RESULTS_FILE" || true)
echo "Packages passed : $PASS_COUNT" | tee -a "$RESULTS_FILE"
echo "Packages failed : $FAIL_COUNT" | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"
echo "Results written to: $RESULTS_FILE"

exit "$TEST_EXIT"
