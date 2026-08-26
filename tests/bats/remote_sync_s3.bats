#!/usr/bin/env bats
# End-to-end s3:// remote-sync tests against a local MinIO server. Exercises
# dumboPush/dumboClone/dumboFetch over a generic S3-compatible object store with
# no cloud dependency. Requires the MinIO binary (run `make minio`); skips
# cleanly when it is absent so the default bats run stays offline.

load helpers

setup() {
    if [ ! -x "$MINIO_BINARY" ]; then
        skip "minio binary not built (run 'make minio')"
    fi

    MINIO_DATA_DIR="$(mktemp -d)"
    MINIO_API_PORT="$(free_port)"
    MINIO_CONSOLE_PORT="$(free_port)"
    S3_BUCKET="dumbo"
    start_minio "$MINIO_DATA_DIR" "$MINIO_API_PORT" "$MINIO_CONSOLE_PORT" "$S3_BUCKET"

    # The dumbodb server reads S3 credentials from the AWS SDK chain, so export
    # the MinIO root credentials as AWS_* before starting it. Isolate the dolt
    # home too.
    export AWS_ACCESS_KEY_ID="$MINIO_ACCESS_KEY"
    export AWS_SECRET_ACCESS_KEY="$MINIO_SECRET_KEY"
    export AWS_REGION="us-east-1"
    DOLT_ISOLATED_HOME="$(mktemp -d)"
    export DOLT_ROOT_PATH="$DOLT_ISOLATED_HOME"

    DUMBODB_DATA_DIR="$(mktemp -d)"
    DUMBODB_PORT="$(free_port)"
    start_dumbodb "$DUMBODB_DATA_DIR" "$DUMBODB_PORT"
}

teardown() {
    stop_dumbodb
    stop_minio
    rm -rf "$DUMBODB_DATA_DIR" "$MINIO_DATA_DIR" "$DOLT_ISOLATED_HOME"
    unset DOLT_ROOT_PATH AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_REGION
}

# s3_url <repo>
# Build a path-style s3:// URL for <repo> in the fixture bucket, routed at MinIO.
s3_url() {
    local repo="$1"
    echo "s3://${S3_BUCKET}/${repo}?endpoint=${MINIO_ENDPOINT}&region=us-east-1&path-style=true"
}

@test 'dumboPush to s3 (MinIO), then dumboClone it back' {
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"
    local url; url="$(s3_url clone-target)"

    run mongo_json "$db_uri" 'db.col.insertOne({_id:1,name:"alice"})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    run mongo_json "$db_uri" "db.runCommand({dumboCommit:1,message:'seed',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${url}'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    run mongo_json "$db_uri" "db.runCommand({dumboPush:1,to:'origin'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .upToDate == false'

    # Clone the pushed bucket path into a new database via admin.
    local admin_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/admin"
    run mongo_json "$admin_uri" "db.runCommand({dumboClone:1,from:'${url}',as:'clonedb'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .db == "clonedb" and .defaultBranch == "main"'

    # The cloned database is readable and contains the seeded document.
    local clone_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/clonedb"
    run mongo_json "$clone_uri" 'db.col.findOne({_id:1})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.name == "alice"'
}

@test 'dumboFetch from s3 (MinIO) updates remote-tracking refs' {
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"
    local url; url="$(s3_url fetch-target)"

    run mongo_json "$db_uri" 'db.col.insertOne({_id:1,name:"alice"})'
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboCommit:1,message:'seed',author:'t <t@t>'})"
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboRemote:1,action:'add',name:'origin',url:'${url}'})"
    [ "$status" -eq 0 ]
    run mongo_json "$db_uri" "db.runCommand({dumboPush:1,to:'origin'})"
    [ "$status" -eq 0 ]

    # A second database fetches from the same bucket path.
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
