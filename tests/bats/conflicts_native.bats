#!/usr/bin/env bats
# Conflict native-persistence tests.
#
# Verifies the 1-1 mapping between the doltConflicts wire command and the
# dolt_conflicts_<collection> SQL table written by the ArtifactMap persistence
# layer.  For each operation (merge, cherry-pick, rebase) the test:
#   1. Creates a conflict scenario via the MongoDB wire protocol.
#   2. Asserts the doltConflicts command returns the expected count.
#   3. Stops dumbodb and queries the dolt_conflicts SQL table directly.
#   4. Asserts the SQL row count matches the wire-protocol count.

load helpers

DUMBODB_PORT=37029

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

# ---------------------------------------------------------------------------
# Helper: run a mongosh eval and capture stdout.  Status is NOT checked here
# because some commands (e.g. doltMerge on conflict) return ok:0, which
# causes mongosh to exit non-zero via --eval even though it is expected.
# Callers check individual fields with jq.
# ---------------------------------------------------------------------------
mongosh_eval() {
    local db_name="$1"
    local js="$2"
    mongosh "mongodb://127.0.0.1:${DUMBODB_PORT}/${db_name}" \
        --quiet --eval "$js" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Test 1: doltMerge conflict → doltConflicts wire == dolt_conflicts SQL
# ---------------------------------------------------------------------------
@test 'merge conflict: doltConflicts wire count matches dolt_conflicts SQL count' {
    local main_db="test__d_main"

    # ---- Setup ---------------------------------------------------------------
    # C1: insert {_id:1, v:1} on main branch.
    run mongosh_eval "$main_db" '
        db.items.insertOne({_id: 1, v: 1});
        db.runCommand({dumbodbCommit: 1, message: "C1", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # Create "feature" branch from main.
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltBranch: 1, branch: "feature"}))
    '
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    # C2: advance main — update _id:1 to v:10.
    run mongosh_eval "$main_db" '
        db.items.updateOne({_id: 1}, {$set: {v: 10}});
        db.runCommand({dumbodbCommit: 1, message: "C2-main", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # C3: advance feature — update _id:1 to v:20.
    run mongosh_eval "test__d_feature" '
        db.items.updateOne({_id: 1}, {$set: {v: 20}});
        db.runCommand({dumbodbCommit: 1, message: "C3-feat", author: "bob <b@t>"});
    '
    [ "$status" -eq 0 ]

    # ---- Trigger conflict: merge feature into main ----------------------------
    run mongosh_eval "$main_db" '
        try { JSON.stringify(db.runCommand({doltMerge: 1, merge_in: "feature", message: "merge", author: "alice <a@t>"})) } catch(e) { JSON.stringify(e.errorResponse) }
    '
    # ok:0 is expected; doltMerge exits non-zero on conflict — status allowed.
    echo "$output" | jq -e '.ok == 0 and (.conflicts | length) > 0'

    # ---- Wire: doltConflicts must report 1 conflict in "items" ---------------
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltConflicts: 1}))
    '
    [ "$status" -eq 0 ]
    local wire_count
    wire_count="$(echo "$output" | jq '[.collections[] | select(.collection == "items") | .conflictCount] | add // 0')"
    [ "$wire_count" -eq 1 ]

    # Get the conflictId from the per-collection detail response (server still up).
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltConflicts: 1, collection: "items"}))
    '
    [ "$status" -eq 0 ]
    local wire_id
    wire_id="$(echo "$output" | jq -r '.conflicts[0].conflictId')"
    [ -n "$wire_id" ]
    [ "$wire_id" != "null" ]

    # ---- SQL: stop server, query dolt_conflicts_items ------------------------
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    run dolt sql -q 'select count(*) from dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_count
    sql_count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_count" -eq "$wire_count" ]

    # Assert the wire conflictId matches the _id column in the SQL table (ID alignment).
    run dolt sql -q 'SELECT LOWER(HEX(our__id)) FROM dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_id
    sql_id="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_id" = "$wire_id" ]
}

# ---------------------------------------------------------------------------
# Test 2: doltCherryPick conflict → doltConflicts wire == dolt_conflicts SQL
# ---------------------------------------------------------------------------
@test 'cherry-pick conflict: doltConflicts wire count matches dolt_conflicts SQL count' {
    local main_db="test__d_main"

    # ---- Setup ---------------------------------------------------------------
    # C1: insert {_id:1, v:1} on main.
    run mongosh_eval "$main_db" '
        db.items.insertOne({_id: 1, v: 1});
        db.runCommand({dumbodbCommit: 1, message: "C1", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # Create "feature" branch from main C1.
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltBranch: 1, branch: "feature"}))
    '
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    # C2 on feature: update _id:1 to v:feature.
    run mongosh_eval "test__d_feature" '
        db.items.updateOne({_id: 1}, {$set: {v: 99}});
        JSON.stringify(db.runCommand({dumbodbCommit: 1, message: "C2-feat", author: "bob <b@t>"}))
    '
    [ "$status" -eq 0 ]
    local hash_c2
    hash_c2="$(echo "$output" | jq -r '.commitId')"
    [ -n "$hash_c2" ] && [ "$hash_c2" != "null" ]

    # C3 on main: update _id:1 to v:main — creates divergence.
    run mongosh_eval "$main_db" '
        db.items.updateOne({_id: 1}, {$set: {v: 42}});
        db.runCommand({dumbodbCommit: 1, message: "C3-main", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # ---- Trigger conflict: cherry-pick C2 onto main --------------------------
    run mongosh_eval "$main_db" \
        "try { JSON.stringify(db.runCommand({doltCherryPick: 1, commit: '${hash_c2}'})) } catch(e) { JSON.stringify(e.errorResponse) }"
    # ok:0 expected on conflict.
    echo "$output" | jq -e '.ok == 0 and (.conflicts | length) > 0'

    # ---- Wire: doltConflicts must report 1 conflict in "items" ---------------
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltConflicts: 1}))
    '
    [ "$status" -eq 0 ]
    local wire_count
    wire_count="$(echo "$output" | jq '[.collections[] | select(.collection == "items") | .conflictCount] | add // 0')"
    [ "$wire_count" -eq 1 ]

    # Get the conflictId from the per-collection detail response (server still up).
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltConflicts: 1, collection: "items"}))
    '
    [ "$status" -eq 0 ]
    local wire_id
    wire_id="$(echo "$output" | jq -r '.conflicts[0].conflictId')"
    [ -n "$wire_id" ]
    [ "$wire_id" != "null" ]

    # ---- SQL: stop server, query dolt_conflicts_items ------------------------
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    run dolt sql -q 'select count(*) from dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_count
    sql_count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_count" -eq "$wire_count" ]

    # Assert the wire conflictId matches the _id column in the SQL table (ID alignment).
    run dolt sql -q 'SELECT LOWER(HEX(our__id)) FROM dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_id
    sql_id="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_id" = "$wire_id" ]
}

# ---------------------------------------------------------------------------
# Test 3: doltRebase conflict → doltConflicts wire == dolt_conflicts SQL
# ---------------------------------------------------------------------------
@test 'rebase conflict: doltConflicts wire count matches dolt_conflicts SQL count' {
    local main_db="test__d_main"

    # ---- Setup ---------------------------------------------------------------
    # C1: insert {_id:1, v:1} on main.
    run mongosh_eval "$main_db" '
        db.items.insertOne({_id: 1, v: 1});
        db.runCommand({dumbodbCommit: 1, message: "C1", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # Create "feature" branch from main C1.
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltBranch: 1, branch: "feature"}))
    '
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    # C2 on feature: update _id:1 to v:feature — the commit to be replayed.
    run mongosh_eval "test__d_feature" '
        db.items.updateOne({_id: 1}, {$set: {v: 55}});
        db.runCommand({dumbodbCommit: 1, message: "C2-feat", author: "bob <b@t>"});
    '
    [ "$status" -eq 0 ]

    # C3 on main: update _id:1 to v:main — creates conflict when C2 is replayed.
    run mongosh_eval "$main_db" '
        db.items.updateOne({_id: 1}, {$set: {v: 77}});
        db.runCommand({dumbodbCommit: 1, message: "C3-main", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # ---- Trigger conflict: rebase feature onto main --------------------------
    run mongosh_eval "test__d_feature" '
        try { JSON.stringify(db.runCommand({doltRebase: 1, onto: "main", author: "bob <b@t>"})) } catch(e) { JSON.stringify(e.errorResponse) }
    '
    # ok:0 expected on conflict.
    echo "$output" | jq -e '.ok == 0 and (.conflicts | length) > 0'

    # ---- Wire: doltConflicts must report 1 conflict in "items" ---------------
    run mongosh_eval "test__d_feature" '
        JSON.stringify(db.runCommand({doltConflicts: 1}))
    '
    [ "$status" -eq 0 ]
    local wire_count
    wire_count="$(echo "$output" | jq '[.collections[] | select(.collection == "items") | .conflictCount] | add // 0')"
    [ "$wire_count" -eq 1 ]

    # Get the conflictId from the per-collection detail response (server still up).
    run mongosh_eval "test__d_feature" '
        JSON.stringify(db.runCommand({doltConflicts: 1, collection: "items"}))
    '
    [ "$status" -eq 0 ]
    local wire_id
    wire_id="$(echo "$output" | jq -r '.conflicts[0].conflictId')"
    [ -n "$wire_id" ]
    [ "$wire_id" != "null" ]

    # ---- SQL: stop server, query dolt_conflicts_items on feature branch ------
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    # Switch to "feature" branch whose working set has the conflict artifacts.
    run dolt checkout feature
    [ "$status" -eq 0 ]

    run dolt sql -q 'select count(*) from dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_count
    sql_count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_count" -eq "$wire_count" ]

    # Assert the wire conflictId matches the _id column in the SQL table (ID alignment).
    run dolt sql -q 'SELECT LOWER(HEX(our__id)) FROM dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_id
    sql_id="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_id" = "$wire_id" ]
}

# ---------------------------------------------------------------------------
# Test 4: doltRevert conflict → doltConflicts wire == dolt_conflicts SQL
# ---------------------------------------------------------------------------
@test 'revert conflict: doltConflicts wire count matches dolt_conflicts SQL count' {
    local main_db="test__d_main"

    # ---- Setup ---------------------------------------------------------------
    # C1: insert {_id:1, v:1} on main.
    run mongosh_eval "$main_db" '
        db.items.insertOne({_id: 1, v: 1});
        db.runCommand({dumbodbCommit: 1, message: "C1", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # C2: add {_id:2, v:2} — this is the commit we will revert.
    run mongosh_eval "$main_db" '
        db.items.insertOne({_id: 2, v: 2});
        JSON.stringify(db.runCommand({dumbodbCommit: 1, message: "C2-add-two", author: "bob <b@t>"}))
    '
    [ "$status" -eq 0 ]
    local hash_c2
    hash_c2="$(echo "$output" | jq -r '.commitId')"
    [ -n "$hash_c2" ] && [ "$hash_c2" != "null" ]

    # C3: modify {_id:2, v:99} on main — creates conflict when we revert C2
    # (revert would delete _id:2, but main has since modified it, so conflict).
    run mongosh_eval "$main_db" '
        db.items.updateOne({_id: 2}, {$set: {v: 99}});
        db.runCommand({dumbodbCommit: 1, message: "C3-modify-two", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # ---- Trigger conflict: revert C2 on main ---------------------------------
    run mongosh_eval "$main_db" \
        "try { JSON.stringify(db.runCommand({doltRevert: 1, commit: '${hash_c2}'})) } catch(e) { JSON.stringify(e.errorResponse) }"
    # ok:0 expected on conflict.
    echo "$output" | jq -e '.ok == 0 and (.conflicts | length) > 0'

    # ---- Wire: doltConflicts must report 1 conflict in "items" ---------------
    run mongosh_eval "$main_db" '
        JSON.stringify(db.runCommand({doltConflicts: 1}))
    '
    [ "$status" -eq 0 ]
    local wire_count
    wire_count="$(echo "$output" | jq '[.collections[] | select(.collection == "items") | .conflictCount] | add // 0')"
    [ "$wire_count" -eq 1 ]

    # ---- SQL: stop server, query dolt_conflicts_items ------------------------
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    run dolt sql -q 'select count(*) from dolt_conflicts_items' --result-format csv
    [ "$status" -eq 0 ]
    local sql_count
    sql_count="$(echo "$output" | tail -1 | tr -d '[:space:]')"
    [ "$sql_count" -eq "$wire_count" ]
}
