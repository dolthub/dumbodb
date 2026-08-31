#!/usr/bin/env bats
# End-to-end gs:// remote-sync tests against a local fake-gcs-server emulator.
# Exercises dumboPush/dumboClone/dumboFetch over a Google Cloud Storage endpoint
# with no cloud dependency. Requires the fake-gcs-server binary (run
# `make fakegcs`); skips cleanly when it is absent so the default bats run stays
# offline. In CI a Docker service container can provide the emulator instead.

load helpers

setup() {
    # Missing infra is a hard failure in CI, a skip only for local developers.
    require_infra "$FAKEGCS_BINARY" "fake-gcs-server" "fakegcs" || return 1

    FAKEGCS_DATA_DIR="$(mktemp -d)"
    FAKEGCS_PORT="$(free_port)"
    GS_BUCKET="dumbo"
    start_fakegcs "$FAKEGCS_DATA_DIR" "$FAKEGCS_PORT" "$GS_BUCKET"

    # The dumbodb server's GCS SDK is routed at the emulator via
    # STORAGE_EMULATOR_HOST. Isolate the dolt home too.
    export STORAGE_EMULATOR_HOST="$FAKEGCS_ENDPOINT"
    DOLT_ISOLATED_HOME="$(mktemp -d)"
    export DOLT_ROOT_PATH="$DOLT_ISOLATED_HOME"

    DUMBODB_DATA_DIR="$(mktemp -d)"
    DUMBODB_PORT="$(free_port)"
    start_dumbodb "$DUMBODB_DATA_DIR" "$DUMBODB_PORT"
}

teardown() {
    stop_dumbodb
    stop_fakegcs
    rm -rf "$DUMBODB_DATA_DIR" "$FAKEGCS_DATA_DIR" "$DOLT_ISOLATED_HOME"
    unset DOLT_ROOT_PATH STORAGE_EMULATOR_HOST
}

@test 'dumboPush to gs (fake-gcs), then dumboClone it back' {
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"
    local url="gs://${GS_BUCKET}/clone-target"

    run mongo_json "$db_uri" 'db.col.insertOne({_id:1,name:"alice"})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    run mongo_json "$db_uri" "db.runCommand({dumboCommit:1,message:'seed',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${url}'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboPush:1,to:'origin',refSpec:'main'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .upToDate == false'

    local admin_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/admin"
    run mongo_json "$admin_uri" "db.runCommand({dumboClone:1,from:'${url}',as:'clonedb'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .db == "clonedb" and .defaultBranch == "main"'

    local clone_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/clonedb"
    run mongo_json "$clone_uri" 'db.col.findOne({_id:1})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.name == "alice"'
}

@test 'dumboFetch from gs (fake-gcs) updates remote-tracking refs' {
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"
    local url="gs://${GS_BUCKET}/fetch-target"

    run mongo_json "$db_uri" 'db.col.insertOne({_id:1,name:"alice"})'
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboCommit:1,message:'seed',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${url}'})"
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboPush:1,to:'origin',refSpec:'main'})"
    [ "$status" -eq 0 ]

    local db2_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/other"
    run mongo_json "$db2_uri" 'db.col.insertOne({_id:9,name:"seed-other"})'
    [ "$status" -eq 0 ]
    run mongo_json "$db2_uri" "db.runCommand({dumboCommit:1,message:'seed other',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    run mongo_json "$db2_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${url}'})"
    [ "$status" -eq 0 ]

    run mongo_json "$db2_uri" "db.runCommand({dumboFetch:1,from:'origin'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'
    echo "$output" | jq -e '[.branches[].branch] | index("main") != null'
}
