#!/usr/bin/env bash
# Bats test helpers for docudolt integration tests.

DOCUDOLT_BINARY="${DOCUDOLT_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/docudolt}"
DOCUDOLT_PID=""

# start_docudolt <data-dir> <port>
# Build and start the docudolt server, wait until it accepts connections.
start_docudolt() {
    local data_dir="$1"
    local port="$2"
    local addr="127.0.0.1:${port}"
    local log_file="${data_dir}.log"

    mkdir -p "$data_dir"

    "$DOCUDOLT_BINARY" --addr "$addr" --data-dir "$data_dir" >"$log_file" 2>&1 &
    DOCUDOLT_PID=$!

    # Wait up to 30 seconds for Docudolt to accept connections.
    local ready=0
    for i in $(seq 1 30); do
        if nc -z 127.0.0.1 "$port" 2>/dev/null; then
            ready=1
            break
        fi
        if ! kill -0 "$DOCUDOLT_PID" 2>/dev/null; then
            echo "ERROR: Docudolt exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: Docudolt failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$DOCUDOLT_PID" 2>/dev/null || true
        return 1
    fi
}

# stop_docudolt
# Kill the docudolt process cleanly.
stop_docudolt() {
    if [ -n "$DOCUDOLT_PID" ]; then
        kill "$DOCUDOLT_PID" 2>/dev/null || true
        wait "$DOCUDOLT_PID" 2>/dev/null || true
        DOCUDOLT_PID=""
    fi
}

# setup_dolt_hack <data-dir>
# Given a docudolt data dir, create the .dolt directory structure so that
# dolt commands run from <data-dir>/.. will see the docudolt data.
#
# Docudolt stores its default database in <data-dir>/test/.
# We symlink <data-dir>/../.dolt/noms -> <data-dir>/test so dolt can read it.
setup_dolt_hack() {
    local data_dir="$1"
    local parent_dir
    parent_dir="$(cd "$data_dir/.." && pwd)"
    local dolt_dir="${parent_dir}/.dolt"

    mkdir -p "$dolt_dir"

    # Symlink noms to the 'test' db dir (default docudolt database name).
    ln -sfn "${data_dir}/test" "${dolt_dir}/noms"

    # Create repo_state.json so dolt recognises this as a valid repo.
    cat > "${dolt_dir}/repo_state.json" <<'EOF'
{"head":"refs/heads/main","remotes":{},"backups":{},"branches":{}}
EOF
}
