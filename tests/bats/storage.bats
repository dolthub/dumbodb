#!/usr/bin/env bats
# Dolt storage verification tests.
# Verifies that data written through dongo is correctly persisted in Dolt format.

load helpers

DONGO_PORT=37027

setup() {
    # Create a fresh temp dir for each test.
    DONGO_DATA_DIR="$(mktemp -d)"
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
    # Start dongo with a fresh data dir.
    start_dongo "$DONGO_DATA_DIR" "$DONGO_PORT"

    local mongo_uri="mongodb://127.0.0.1:${DONGO_PORT}/test"

    # Insert alice.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"alice",age:30}))'
    [ "$status" -eq 0 ]
    run sh -c "echo '$output' | jq -e '.acknowledged == true and .insertedId != null'"
    [ "$status" -eq 0 ]

    # Insert bob.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"bob",age:25}))'
    [ "$status" -eq 0 ]
    run sh -c "echo '$output' | jq -e '.acknowledged == true and .insertedId != null'"
    [ "$status" -eq 0 ]

    # Stop dongo so data is flushed.
    stop_dongo

    # Set up the dolt symlink hack so dolt can read dongo's data.
    setup_dolt_hack "$DONGO_DATA_DIR"

    local repo_dir
    repo_dir="$(dirname "$DONGO_DATA_DIR")"

    # Verify dolt storage integrity.
    run sh -c "cd '$repo_dir' && dolt fsck"
    [ "$status" -eq 0 ]

    # Verify the two inserted documents appear in dolt sql.
    run sh -c "cd '$repo_dir' && dolt sql -q 'select count(*) from col1' --result-format csv"
    [ "$status" -eq 0 ]
    echo "dolt sql output: $output"
    # The CSV output has a header line and a data line; the count should be 2.
    run sh -c "echo '$output' | tail -1 | tr -d '[:space:]'"
    [ "$output" = "2" ]
}
