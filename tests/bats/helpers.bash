#!/usr/bin/env bash
# Bats test helpers for dongo integration tests.

DONGO_BINARY="${DONGO_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/dongo}"
DONGO_PID=""

# start_dongo <data-dir> <port>
# Build and start the dongo server, wait until it accepts connections.
start_dongo() {
    local data_dir="$1"
    local port="$2"
    local addr="127.0.0.1:${port}"
    local log_file="${data_dir}.log"

    mkdir -p "$data_dir"

    "$DONGO_BINARY" --addr "$addr" --data-dir "$data_dir" >"$log_file" 2>&1 &
    DONGO_PID=$!

    # Wait up to 30 seconds for Dongo to accept connections.
    local ready=0
    for i in $(seq 1 30); do
        if nc -z 127.0.0.1 "$port" 2>/dev/null; then
            ready=1
            break
        fi
        if ! kill -0 "$DONGO_PID" 2>/dev/null; then
            echo "ERROR: Dongo exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: Dongo failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$DONGO_PID" 2>/dev/null || true
        return 1
    fi
}

# stop_dongo
# Kill the dongo process cleanly.
stop_dongo() {
    if [ -n "$DONGO_PID" ]; then
        kill "$DONGO_PID" 2>/dev/null || true
        wait "$DONGO_PID" 2>/dev/null || true
        DONGO_PID=""
    fi
}

# setup_dolt_hack <data-dir>
# Given a dongo data dir, create the .dolt directory structure so that
# dolt commands run from <data-dir>/.. will see the dongo data.
#
# Dongo stores its default database in <data-dir>/test/.
# We symlink <data-dir>/../.dolt/noms -> <data-dir>/test so dolt can read it.
setup_dolt_hack() {
    local data_dir="$1"
    local parent_dir
    parent_dir="$(cd "$data_dir/.." && pwd)"
    local dolt_dir="${parent_dir}/.dolt"

    mkdir -p "$dolt_dir"

    # Symlink noms to the 'test' db dir (default dongo database name).
    ln -sfn "${data_dir}/test" "${dolt_dir}/noms"

    # Create repo_state.json so dolt recognises this as a valid repo.
    cat > "${dolt_dir}/repo_state.json" <<'EOF'
{"head":"refs/heads/main","remotes":{},"backups":{},"branches":{}}
EOF
}
