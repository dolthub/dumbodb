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

@test 'inserted docs appear as added rows in dolt diff' {
    local mongo_uri="mongodb://127.0.0.1:${DONGO_PORT}/test"

    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"alice",age:30}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true and .insertedId != null'

    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col1.insertOne({name:"bob",age:25}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true and .insertedId != null'

    stop_dongo

    setup_dolt_hack "$DONGO_DATA_DIR"

    local repo_dir
    repo_dir="$(dirname "$DONGO_DATA_DIR")"
    cd "$repo_dir"

    run dolt diff --result-format=sql
    [ "$status" -eq 0 ]

    # The table itself should be new.
    [[ "$output" =~ 'CREATE TABLE `col1`' ]] || false

    # Each inserted document should appear as an INSERT with a hex _id.
    local added_rows
    added_rows="$(echo "$output" | grep -c '^INSERT INTO')"
    [ "$added_rows" -eq 2 ]

    # The doc column should contain alice and bob (JSON is escaped in SQL format).
    [[ "$output" =~ '\"name\":\"alice\"' ]] || false
    [[ "$output" =~ '\"name\":\"bob\"' ]] || false
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

@test 'dongoCommit returns non-empty hash' {
    local mongo_uri="mongodb://127.0.0.1:${DONGO_PORT}/test"

    # Insert a document so there is something to commit.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.col.insertOne({x:1}))'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    # Run dongoCommit and capture the result.
    run mongosh "$mongo_uri" --quiet --eval \
        'JSON.stringify(db.runCommand({dongoCommit: 1, message: "my first commit"}))'
    [ "$status" -eq 0 ]

    # Verify ok:1 and a non-empty hash.
    echo "$output" | jq -e '.ok == 1'
    local hash
    hash="$(echo "$output" | jq -r '.hash')"
    [ -n "$hash" ]
    [ "$hash" != "null" ]
    [ "$hash" != "0000000000000000000000000000000000000000" ]
}
