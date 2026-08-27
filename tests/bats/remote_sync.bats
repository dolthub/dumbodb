#!/usr/bin/env bats
# Remote-sync integration tests: push, fetch, and clone between a running
# dumbodb server and a local Dolt remotesrv endpoint. Exercises the http(s)
# gRPC transport end to end with no DoltHub or network dependency.

load helpers

setup() {
    DUMBODB_DATA_DIR="$(mktemp -d)"
    DUMBODB_PORT="$(free_port)"

    REMOTESRV_DIR="$(mktemp -d)"
    REMOTESRV_PORT="$(free_port)"

    # Isolate the server's Dolt home so an insecure http remote needs no
    # credentials and the test never reads the host's `dolt login` state.
    DOLT_ISOLATED_HOME="$(mktemp -d)"
    export DOLT_ROOT_PATH="$DOLT_ISOLATED_HOME"

    start_remotesrv "$REMOTESRV_DIR" "$REMOTESRV_PORT"
    start_dumbodb "$DUMBODB_DATA_DIR" "$DUMBODB_PORT"
}

teardown() {
    stop_dumbodb
    stop_remotesrv
    rm -rf "$DUMBODB_DATA_DIR" "$REMOTESRV_DIR" "$DOLT_ISOLATED_HOME"
    unset DOLT_ROOT_PATH
}

# seed_and_push <repo-path>
# Insert a document into test.col, commit it, add an origin remote pointing at
# the remotesrv repo, and push. Asserts each step succeeds.
seed_and_push() {
    local repo="$1"
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"
    local remote_url="${REMOTESRV_URL}/${repo}"

    run mongo_json "$db_uri" 'db.col.insertOne({_id:1,name:"alice"})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    run mongo_json "$db_uri" "db.runCommand({dumboCommit:1,message:'seed',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${remote_url}'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboPush:1,to:'origin'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'
}

@test 'dumboPush to remotesrv, then dumboClone it back' {
    seed_and_push "clone-target"

    # Clone the pushed repo into a new server-side database via admin.
    local admin_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/admin"
    local remote_url="${REMOTESRV_URL}/clone-target"
    run mongo_json "$admin_uri" "db.runCommand({dumboClone:1,from:'${remote_url}',as:'clonedb'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'
    echo "$output" | jq -e '.db == "clonedb"'
    echo "$output" | jq -e '.defaultBranch == "main"'

    # The cloned database is readable and contains the seeded document.
    local clone_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/clonedb"
    run mongo_json "$clone_uri" 'db.col.findOne({_id:1})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.name == "alice"'
}

@test 'dumboFetch from remotesrv updates remote-tracking refs' {
    seed_and_push "fetch-target"

    # A second database fetches from the same remotesrv repo.
    local db2_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/other"
    local remote_url="${REMOTESRV_URL}/fetch-target"

    run mongo_json "$db2_uri" 'db.col.insertOne({_id:9,name:"seed-other"})'
    [ "$status" -eq 0 ]
    run mongo_json "$db2_uri" "db.runCommand({dumboCommit:1,message:'seed other',author:'t <t@t>'})"
    [ "$status" -eq 0 ]

    run mongo_json "$db2_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${remote_url}'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db2_uri" "db.runCommand({dumboFetch:1,from:'origin'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'
    # main was fetched into the remote-tracking refs.
    echo "$output" | jq -e '[.branches[].branch] | index("main") != null'
}

@test 'independent dolt clone reads a dumbodb-pushed remotesrv repo' {
    seed_and_push "dolt-clone-target"

    # Point stock dolt at the same remotesrv repo over insecure http and clone.
    local clone_dir="${REMOTESRV_DIR}.doltclone"
    rm -rf "$clone_dir"
    run dolt clone "http://127.0.0.1:${REMOTESRV_PORT}/dolt-clone-target" "$clone_dir"
    [ "$status" -eq 0 ]

    cd "$clone_dir"
    run dolt sql -q 'select count(*) from col' --result-format csv
    [ "$status" -eq 0 ]
    local count
    count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$count" = "1" ]
}

@test 'upstream: push/fetch default to the tracked remote after an explicit push' {
    # seed_and_push does an explicit push to origin, which records the upstream.
    seed_and_push "upstream-target"
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"

    # Push with no target follows the recorded upstream (origin).
    run mongo_json "$db_uri" "db.runCommand({dumboPush:1})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .remote == "origin"'

    # Fetch with no remote follows it too.
    run mongo_json "$db_uri" "db.runCommand({dumboFetch:1})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .remote == "origin"'
}
