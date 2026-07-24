#!/usr/bin/env bash
# Bats test helpers for dumbodb integration tests.

DUMBODB_BINARY="${DUMBODB_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/dumbodb}"
DUMBODB_PID=""

# port_open <host> <port>
# True when a TCP connection to host:port succeeds. Uses nc when
# available, falling back to bash's built-in /dev/tcp so the suite
# also runs on machines without netcat. The fallback runs in a child
# bash so it cannot disturb bats's internal file descriptors (bats
# reserves FD 3 for its test protocol).
port_open() {
    local host="$1"
    local port="$2"
    if command -v nc >/dev/null 2>&1; then
        nc -z "$host" "$port" 2>/dev/null
    else
        bash -c "exec 9<>'/dev/tcp/${host}/${port}'" 2>/dev/null
    fi
}

# free_port
# Echo a random TCP port that nothing is currently listening on, chosen
# from a range BELOW the Linux ephemeral range (default 32768-60999).
# A server port inside the ephemeral range can collide with an outbound
# client connection that the kernel assigned the same local port to,
# causing an intermittent "bind: address already in use" -- the cause of
# flaky bats failures when files hardcoded ports like 37027-37030. Tests
# should call this per run instead of hardcoding a port.
free_port() {
    local p
    for _ in $(seq 1 50); do
        # 20000-32767: above the well-known/registered-low range, below
        # the ephemeral range.
        p=$(( (RANDOM % 12767) + 20000 ))
        if ! port_open 127.0.0.1 "$p"; then
            echo "$p"
            return 0
        fi
    done
    echo "ERROR: could not find a free port" >&2
    return 1
}

# start_dumbodb <data-dir> <port>
# Build and start the dumbodb server, wait until it accepts connections.
start_dumbodb() {
    local data_dir="$1"
    local port="$2"
    local addr="127.0.0.1:${port}"
    local log_file="${data_dir}.log"

    mkdir -p "$data_dir"

    "$DUMBODB_BINARY" --addr "$addr" --data-dir "$data_dir" >"$log_file" 2>&1 &
    DUMBODB_PID=$!

    # Wait up to 30 seconds for dumbodb to accept connections.
    local ready=0
    for i in $(seq 1 30); do
        if port_open 127.0.0.1 "$port"; then
            ready=1
            break
        fi
        if ! kill -0 "$DUMBODB_PID" 2>/dev/null; then
            echo "ERROR: dumbodb exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: dumbodb failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$DUMBODB_PID" 2>/dev/null || true
        return 1
    fi
}

# stop_dumbodb
# Kill the dumbodb process cleanly.
stop_dumbodb() {
    if [ -n "$DUMBODB_PID" ]; then
        kill "$DUMBODB_PID" 2>/dev/null || true
        wait "$DUMBODB_PID" 2>/dev/null || true
        DUMBODB_PID=""
    fi
}

# setup_dolt_hack <data-dir>
# Given a dumbodb data dir, create the .dolt directory structure so that
# dolt commands run from <data-dir>/.. will see the dumbodb data.
#
# dumbodb stores its default database in <data-dir>/test/.
# We symlink <data-dir>/../.dolt/noms -> <data-dir>/test so dolt can read it.
setup_dolt_hack() {
    local data_dir="$1"
    local parent_dir
    parent_dir="$(cd "$data_dir/.." && pwd)"
    local dolt_dir="${parent_dir}/.dolt"

    mkdir -p "$dolt_dir"

    # Symlink noms to the 'test' db dir (default dumbodb database name).
    ln -sfn "${data_dir}/test" "${dolt_dir}/noms"

    # Create repo_state.json so dolt recognises this as a valid repo.
    cat > "${dolt_dir}/repo_state.json" <<'EOF'
{"head":"refs/heads/main","remotes":{},"backups":{},"branches":{}}
EOF
}
