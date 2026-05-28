#!/usr/bin/env bats
# dumboGC wire-command tests. Verifies that the runCommand decodes
# correctly, reports the expected response fields, and actually
# shrinks the chunk count after a delete-everything workload.

load helpers

DUMBODB_PORT=37030

setup() {
    DUMBODB_DATA_DIR="$(mktemp -d)"
    start_dumbodb "$DUMBODB_DATA_DIR" "$DUMBODB_PORT"
}

teardown() {
    stop_dumbodb
    rm -rf "$DUMBODB_DATA_DIR"
    local parent_dir
    parent_dir="$(dirname "$DUMBODB_DATA_DIR")"
    rm -rf "${parent_dir}/.dolt"
}

mongosh_eval() {
    local db_name="$1"
    local js="$2"
    mongosh "mongodb://127.0.0.1:${DUMBODB_PORT}" \
        --quiet --eval "db = db.getSiblingDB('${db_name}'); ${js}" 2>/dev/null || true
}

@test 'dumboGC: 1 returns the expected fields' {
    # Establish the database with one insert so the dbState exists.
    run mongosh_eval "gcwire" 'db.items.insertOne({_id: 1, v: "hello"});'
    [ "$status" -eq 0 ]

    run mongosh_eval "gcwire" 'JSON.stringify(db.runCommand({dumboGC: 1}));'
    [ "$status" -eq 0 ]

    # Required fields.
    echo "$output" | jq -e '.ok == 1'
    echo "$output" | jq -e '.db == "gcwire"'
    echo "$output" | jq -e '.mode == "default"'
    echo "$output" | jq -e '(.durationMs | type) == "number"'
    echo "$output" | jq -e '(.sizeBefore | type) == "number"'
    echo "$output" | jq -e '(.sizeAfter | type) == "number"'
    echo "$output" | jq -e '(.chunksBefore | type) == "number"'
    echo "$output" | jq -e '(.chunksAfter | type) == "number"'
}

@test 'dumboGC: full mode is accepted and echoed in the response' {
    run mongosh_eval "gcwirefull" 'db.items.insertOne({_id: 1, v: "hello"});'
    [ "$status" -eq 0 ]

    run mongosh_eval "gcwirefull" 'JSON.stringify(db.runCommand({dumboGC: 1, mode: "full"}));'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .mode == "full"'
}

@test 'dumboGC: unknown mode returns an error' {
    run mongosh_eval "gcwirebad" 'db.items.insertOne({_id: 1, v: "hello"});'
    [ "$status" -eq 0 ]

    run mongosh_eval "gcwirebad" \
        'try { JSON.stringify(db.runCommand({dumboGC: 1, mode: "bogus"})) } catch(e) { JSON.stringify(e.errorResponse) }'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 0'
    echo "$output" | jq -e '.errmsg | contains("unknown mode")'
}

@test 'dumboGC: default mode shrinks chunk count after delete-everything workload' {
    local db_name="gcwireshrink"

    # Insert a workload, then delete everything. Auto-commit makes every
    # op produce a new commit; the delete-all leaves the old per-doc
    # chunks unreachable from any branch ref.
    run mongosh_eval "$db_name" '
        for (let i = 0; i < 100; i++) {
            db.items.insertOne({_id: i, payload: "padding-payload-bytes-" + i});
        }
        db.items.deleteMany({});
    '
    [ "$status" -eq 0 ]

    run mongosh_eval "$db_name" 'JSON.stringify(db.runCommand({dumboGC: 1}));'
    [ "$status" -eq 0 ]

    local before after
    before="$(echo "$output" | jq '.chunksBefore')"
    after="$(echo "$output" | jq '.chunksAfter')"
    [ "$after" -lt "$before" ]
}
