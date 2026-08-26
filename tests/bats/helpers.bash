#!/usr/bin/env bash
# Bats test helpers for dumbodb integration tests.

DUMBODB_BINARY="${DUMBODB_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/dumbodb}"
DUMBODB_PID=""

# in_ci: true when running under CI. GitHub Actions and most CI systems set
# CI=true; DUMBO_REQUIRE_INFRA=1 forces the same behavior anywhere.
in_ci() {
    [ -n "${CI:-}" ] || [ -n "${DUMBO_REQUIRE_INFRA:-}" ]
}

# require_infra <path> <name> <make-target>
# Guards a test on an infra binary. In CI a missing binary is a hard FAILURE
# (tests must never fail-green on absent infrastructure); locally it skips so a
# developer without the binary can still run the rest of the suite. Call from a
# test's setup(); on the failure path it returns non-zero so setup fails, and on
# the local path it invokes bats `skip` directly (so it must be called from
# setup/test context, not a nested subshell).
require_infra() {
    local path="$1"
    local name="$2"
    local target="$3"

    if [ -x "$path" ]; then
        return 0
    fi

    if in_ci; then
        echo "ERROR: $name not found at $path; CI must provide it (run 'make ${target}' or provision it)" >&2
        return 1
    fi

    skip "${name} not built (run 'make ${target}')"
}

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

# mongo_json <uri> <js-expression>
# Evaluate a single JavaScript expression against <uri> and print its value as
# JSON on stdout. Wraps the result in print(JSON.stringify(...)); quit(0) so the
# process exits 0 with just the JSON: mongosh 2.10.x throws a spurious
# "getAiAgent is not a function" at shutdown and otherwise exits non-zero even on
# success. Pass the expression using single-quoted JS string literals so its
# quotes survive the shell (e.g. db.runCommand({dumboRemote:1,action:'add'})).
mongo_json() {
    local uri="$1"
    local expr="$2"
    mongosh "$uri" --quiet --eval "print(JSON.stringify(${expr})); quit(0)"
}

# Dolt remotesrv fixture: a local push/fetch/clone endpoint. Built from the
# pinned dolt module (see `make remotesrv`) so its chunk protocol matches the
# dolt version compiled into dumbodb. Started like the dumbodb server so remote-
# sync tests have a hermetic gRPC remote with no DoltHub or network dependency.
REMOTESRV_BINARY="${REMOTESRV_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/remotesrv}"
REMOTESRV_PID=""
REMOTESRV_URL=""

# start_remotesrv <root-dir> <port>
# Start remotesrv serving from <root-dir> with gRPC and http multiplexed on one
# port, wait until it accepts connections, and set REMOTESRV_URL to the base
# endpoint (http://127.0.0.1:<port>). A repo is created on demand under
# <root-dir> the first time something is pushed to http://.../<repo>.
start_remotesrv() {
    local root_dir="$1"
    local port="$2"
    local log_file="${root_dir}.remotesrv.log"

    mkdir -p "$root_dir"

    if [ ! -x "$REMOTESRV_BINARY" ]; then
        echo "ERROR: remotesrv binary not found at $REMOTESRV_BINARY (run 'make remotesrv')" >&2
        return 1
    fi

    # Single-port multiplexing: http-port == grpc-port. Pass only the host in
    # --http-host; remotesrv appends ":<http-port>" itself to form the authority
    # in the URLs it generates, so they point back at this listener.
    "$REMOTESRV_BINARY" \
        --dir "$root_dir" \
        --http-port "$port" \
        --grpc-port "$port" \
        --http-host "127.0.0.1" \
        >"$log_file" 2>&1 &
    REMOTESRV_PID=$!

    local ready=0
    for _ in $(seq 1 30); do
        if port_open 127.0.0.1 "$port"; then
            ready=1
            break
        fi
        if ! kill -0 "$REMOTESRV_PID" 2>/dev/null; then
            echo "ERROR: remotesrv exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: remotesrv failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$REMOTESRV_PID" 2>/dev/null || true
        return 1
    fi

    REMOTESRV_URL="http://127.0.0.1:${port}"
}

# stop_remotesrv
# Kill the remotesrv process cleanly.
stop_remotesrv() {
    if [ -n "$REMOTESRV_PID" ]; then
        kill "$REMOTESRV_PID" 2>/dev/null || true
        wait "$REMOTESRV_PID" 2>/dev/null || true
        REMOTESRV_PID=""
    fi
    REMOTESRV_URL=""
}

# MinIO fixture: a local S3-compatible object store for end-to-end s3:// remote
# tests. Downloaded on demand by `make minio` (kept out of the default bats
# target); s3 tests skip when the binary is absent. Uses fixed root credentials
# that tests also pass to the dumbodb server as AWS_* for the SDK credential
# chain.
MINIO_BINARY="${MINIO_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/minio}"
MINIO_PID=""
MINIO_ENDPOINT=""
MINIO_ACCESS_KEY="minioadmin"
MINIO_SECRET_KEY="minioadmin"

# start_minio <data-dir> <api-port> <console-port> <bucket>
# Start MinIO serving from <data-dir>, pre-create <bucket> (a top-level directory
# is a bucket in single-drive mode), wait for readiness, and set MINIO_ENDPOINT
# to http://127.0.0.1:<api-port>.
start_minio() {
    local data_dir="$1"
    local api_port="$2"
    local console_port="$3"
    local bucket="$4"
    local log_file="${data_dir}.minio.log"

    if [ ! -x "$MINIO_BINARY" ]; then
        echo "ERROR: minio binary not found at $MINIO_BINARY (run 'make minio')" >&2
        return 1
    fi

    mkdir -p "${data_dir}/${bucket}"

    MINIO_ROOT_USER="$MINIO_ACCESS_KEY" MINIO_ROOT_PASSWORD="$MINIO_SECRET_KEY" \
        "$MINIO_BINARY" server "$data_dir" \
        --address "127.0.0.1:${api_port}" \
        --console-address "127.0.0.1:${console_port}" \
        >"$log_file" 2>&1 &
    MINIO_PID=$!

    local ready=0
    for _ in $(seq 1 30); do
        if port_open 127.0.0.1 "$api_port"; then
            ready=1
            break
        fi
        if ! kill -0 "$MINIO_PID" 2>/dev/null; then
            echo "ERROR: minio exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: minio failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$MINIO_PID" 2>/dev/null || true
        return 1
    fi

    MINIO_ENDPOINT="http://127.0.0.1:${api_port}"
}

# stop_minio
# Kill the MinIO process cleanly.
stop_minio() {
    if [ -n "$MINIO_PID" ]; then
        kill "$MINIO_PID" 2>/dev/null || true
        wait "$MINIO_PID" 2>/dev/null || true
        MINIO_PID=""
    fi
    MINIO_ENDPOINT=""
}

# fake-gcs-server fixture: a local Google Cloud Storage emulator for end-to-end
# gs:// remote tests. Built on demand by `make fakegcs` (kept out of the default
# bats target); gs tests skip when the binary is absent. The GCS SDK is routed
# at it via the STORAGE_EMULATOR_HOST environment variable, which tests export
# into the dumbodb server's environment.
FAKEGCS_BINARY="${FAKEGCS_BINARY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../" && pwd)/.runtime/bin/fake-gcs-server}"
FAKEGCS_PID=""
FAKEGCS_ENDPOINT=""

# start_fakegcs <data-dir> <port> <bucket>
# Start fake-gcs-server on the filesystem backend, pre-create <bucket> (a
# top-level directory under the filesystem root), wait for readiness, and set
# FAKEGCS_ENDPOINT to http://127.0.0.1:<port>.
start_fakegcs() {
    local data_dir="$1"
    local port="$2"
    local bucket="$3"
    local log_file="${data_dir}.fakegcs.log"

    if [ ! -x "$FAKEGCS_BINARY" ]; then
        echo "ERROR: fake-gcs-server not found at $FAKEGCS_BINARY (run 'make fakegcs')" >&2
        return 1
    fi

    mkdir -p "${data_dir}/${bucket}"

    # public-host must be the emulator's own address so upload Location headers
    # point back here rather than at storage.googleapis.com.
    "$FAKEGCS_BINARY" \
        -backend filesystem \
        -filesystem-root "$data_dir" \
        -scheme http \
        -host 127.0.0.1 \
        -port "$port" \
        -public-host "127.0.0.1:${port}" \
        >"$log_file" 2>&1 &
    FAKEGCS_PID=$!

    local ready=0
    for _ in $(seq 1 30); do
        if port_open 127.0.0.1 "$port"; then
            ready=1
            break
        fi
        if ! kill -0 "$FAKEGCS_PID" 2>/dev/null; then
            echo "ERROR: fake-gcs-server exited prematurely. Log:" >&2
            cat "$log_file" >&2
            return 1
        fi
        sleep 1
    done

    if [ "$ready" -eq 0 ]; then
        echo "ERROR: fake-gcs-server failed to start within 30s" >&2
        cat "$log_file" >&2
        kill "$FAKEGCS_PID" 2>/dev/null || true
        return 1
    fi

    FAKEGCS_ENDPOINT="http://127.0.0.1:${port}"
}

# stop_fakegcs
# Kill the fake-gcs-server process cleanly.
stop_fakegcs() {
    if [ -n "$FAKEGCS_PID" ]; then
        kill "$FAKEGCS_PID" 2>/dev/null || true
        wait "$FAKEGCS_PID" 2>/dev/null || true
        FAKEGCS_PID=""
    fi
    FAKEGCS_ENDPOINT=""
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
