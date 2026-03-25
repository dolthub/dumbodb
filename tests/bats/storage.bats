#!/usr/bin/env bats
# Dolt storage verification tests.
# Verifies that data written through dongo is correctly persisted in Dolt format.

load helpers

DONGO_PORT=37027

setup() {
    # Create a fresh temp dir for each test.
    DONGO_DATA_DIR="$(mktemp -d)"

    # Start dongo with a fresh data dir.
    start_dongo "$DONGO_DATA_DIR" "$DONGO_PORT"
}

teardown() {
    stop_dongo
    rm -rf "$DONGO_DATA_DIR"
    # Remove the .dolt dir created by setup_dolt_hack.
    local parent_dir
    parent_dir="$(dirname "$DONGO_DATA_DIR")"
    rm -rf "${parent_dir}/.dolt"
}

@test 'insert two docs, verify dolt storage' {
    local mongo_uri="mongodb://127.0.0.1:${DONGO_PORT}/test"

    # Insert alice.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"alice",age:30}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true and .insertedId != null'

    # Insert bob.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"bob",age:25}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true and .insertedId != null'

    # Stop dongo so data is flushed.
    stop_dongo

    # Set up the dolt symlink hack so dolt can read dongo's data.
    setup_dolt_hack "$DONGO_DATA_DIR"

    local repo_dir
    repo_dir="$(dirname "$DONGO_DATA_DIR")"
    cd "$repo_dir"

    # Verify dolt storage integrity.
    run dolt fsck
    [ "$status" -eq 0 ]

    # Verify the two inserted documents appear in dolt sql.
    run dolt sql -q 'select count(*) from col1' --result-format csv
    [ "$status" -eq 0 ]
    # The CSV output has a header line and a data line; the count should be 2.
    local count
    count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$count" = "2" ]
}

@test 'collection schema has _id and doc columns' {
    run mongosh "mongodb://127.0.0.1:${DONGO_PORT}/test" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"alice",age:30}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true and .insertedId != null'

    stop_dongo

    setup_dolt_hack "$DONGO_DATA_DIR"

    local repo_dir
    repo_dir="$(dirname "$DONGO_DATA_DIR")"
    cd "$repo_dir"

    run dolt sql -q 'show create table col1'

    [ "$status" -eq 0 ]
    [[ "$output" =~ '`_id` varbinary(1024) NOT NULL,' ]] || false
    [[ "$output" =~ '`doc` json NOT NULL' ]] || false
    [[ "$output" =~ 'PRIMARY KEY (`_id`)' ]] || false
}
