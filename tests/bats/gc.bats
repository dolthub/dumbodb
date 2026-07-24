#!/usr/bin/env bats
# dumboGC wire-command tests. Verifies that the runCommand decodes
# correctly, reports the expected response fields, and actually
# shrinks the chunk count after a delete-everything workload.

load helpers

# A fresh free port per test (see helpers.bash free_port) avoids the
# fixed-port "bind: address already in use" flake.
setup() {
    DUMBODB_DATA_DIR="$(mktemp -d)"
    DUMBODB_PORT="$(free_port)"
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

# Sweep validation. Generates a real write workload, then verifies that
# full-mode GC actually rearranges on-disk storage: the journal file
# shrinks dramatically, oldgen table files appear, AND the resulting
# store passes `dolt fsck`. This catches regressions in the safepoint
# orchestration that would silently succeed at the wire level but leave
# the storage untouched or corrupted.
#
# Uses db_name="test" so the standard setup_dolt_hack helper can
# symlink the data dir into a .dolt repo for the fsck step.
@test 'dumboGC: full mode moves chunks to oldgen and store passes dolt fsck' {
    local db_name="test"
    local db_dir="${DUMBODB_DATA_DIR}/${db_name}"
    local journal="${db_dir}/vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"

    # Workload: many inserts in batches with explicit commits in
    # between to push chunks down the commit graph, then delete a
    # large fraction to orphan their chunks. Without commits the
    # journal would only hold working-set state; with commits each
    # batch advances HEAD and produces reachable chunks that
    # full-mode GC can compact / drop.
    run mongosh_eval "$db_name" '
        const N_BATCHES = 5;
        const PER_BATCH = 100;
        for (let b = 0; b < N_BATCHES; b++) {
            const docs = [];
            for (let i = 0; i < PER_BATCH; i++) {
                docs.push({_id: b * PER_BATCH + i, payload: "padding-payload-bytes-batch-" + b + "-doc-" + i});
            }
            db.items.insertMany(docs);
            db.runCommand({dumboCommit: 1, message: "batch-" + b, author: "alice <a@t>"});
        }
        // Delete half so default-mode GC also has something to reclaim.
        db.items.deleteMany({_id: {$lt: 250}});
        db.runCommand({dumboCommit: 1, message: "delete-half", author: "alice <a@t>"});
    '
    [ "$status" -eq 0 ]

    # Snapshot the journal size and oldgen state BEFORE GC.
    [ -f "$journal" ]
    local journal_before
    journal_before="$(stat -c %s "$journal")"
    local oldgen_files_before
    oldgen_files_before="$(find "${db_dir}/oldgen" -maxdepth 1 -type f | wc -l)"

    # Run full-mode GC over the wire.
    run mongosh_eval "$db_name" 'JSON.stringify(db.runCommand({dumboGC: 1, mode: "full"}));'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .mode == "full"'

    local reported_before reported_after
    reported_before="$(echo "$output" | jq '.sizeBefore')"
    reported_after="$(echo "$output" | jq '.sizeAfter')"
    # full-mode GC after a substantial workload should shrink the store.
    [ "$reported_after" -lt "$reported_before" ]

    # Filesystem-level: journal should be MUCH smaller now (chunks
    # migrated out). Full-mode GC may even unlink the journal file
    # entirely once everything migrates to oldgen; treat "no journal"
    # as size zero. Threshold is generous to absorb minor format
    # overhead; the test workload alone wrote >50KB to the journal.
    local journal_after
    if [ -f "$journal" ]; then
        journal_after="$(stat -c %s "$journal")"
    else
        journal_after=0
    fi
    [ "$journal_before" -gt 50000 ]
    [ "$journal_after" -lt 10000 ]

    # oldgen now has at least one table file (it was empty before
    # any GC ran).
    local oldgen_files_after
    oldgen_files_after="$(find "${db_dir}/oldgen" -maxdepth 1 -type f | wc -l)"
    [ "$oldgen_files_before" -eq 0 ]
    [ "$oldgen_files_after" -gt 0 ]

    # Integrity check: stop dumbodb, point dolt at the data dir, and
    # run fsck. fsck walks every chunk and verifies refs resolve --
    # a GC bug that leaves dangling refs or corrupted table files
    # surfaces here even when the wire response looks fine.
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    run dolt fsck
    [ "$status" -eq 0 ]
}

# Branch deletion + second GC reclaims branch-only chunks. The first
# GC pass (default mode) compacts the steady-state workload into
# oldgen archives. Then we delete the feature branch -- the chunks
# reachable only from feature become unreachable. A second full-mode
# GC reclaims them, dropping both the chunk count and the on-disk
# size by 2x or more vs. the first GC's post-state.
@test 'dumboGC: branch delete + second full GC reclaims branch-only chunks' {
    local db_name="test"
    local db_dir="${DUMBODB_DATA_DIR}/${db_name}"
    local journal="${db_dir}/vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"

    # Workload sized so feature-only chunks dominate: small main
    # baseline + a much larger feature branch. After branch delete,
    # the second GC should reclaim well over half of post-first-GC
    # chunks and bytes.
    run mongosh_eval "$db_name" '
        // Small main baseline: 2 batches of 50.
        for (let b = 0; b < 2; b++) {
            const docs = [];
            for (let i = 0; i < 50; i++) {
                docs.push({_id: b * 50 + i, payload: "main-payload-" + b + "-" + i});
            }
            db.items.insertMany(docs);
            db.runCommand({dumboCommit: 1, message: "main-batch-" + b, author: "alice <a@t>"});
        }
    '
    [ "$status" -eq 0 ]

    # Create feature branch from current main HEAD.
    run mongosh_eval "$db_name" '
        JSON.stringify(db.runCommand({doltBranch: 1, branch: "feature"}))
    '
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    # Heavy feature workload: 10 batches of 200 with longer payloads.
    # Distinct keys + payloads ensure none of these chunks are shared
    # with main.
    run mongosh_eval "${db_name}@feature" '
        const FEAT_PAD = "feat-padding-bytes-repeated-to-make-the-chunks-substantial-".repeat(8);
        for (let b = 0; b < 10; b++) {
            const docs = [];
            for (let i = 0; i < 200; i++) {
                const id = 100000 + b * 200 + i;
                docs.push({_id: id, payload: FEAT_PAD + "-batch-" + b + "-doc-" + i});
            }
            db.items.insertMany(docs);
            db.runCommand({dumboCommit: 1, message: "feat-batch-" + b, author: "bob <b@t>"});
        }
    '
    [ "$status" -eq 0 ]

    # First GC pass (default mode). Steady-state: compacts current
    # workload into an oldgen archive.
    run mongosh_eval "$db_name" 'JSON.stringify(db.runCommand({dumboGC: 1}));'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'
    local chunks_after_1 size_after_1
    chunks_after_1="$(echo "$output" | jq '.chunksAfter')"
    size_after_1="$(echo "$output" | jq '.sizeAfter')"

    # Filesystem steady-state after first GC: journal small, exactly
    # one .darc file in oldgen.
    local journal_size_1
    if [ -f "$journal" ]; then
        journal_size_1="$(stat -c %s "$journal")"
    else
        journal_size_1=0
    fi
    [ "$journal_size_1" -lt 10000 ]

    local darc_count_1
    darc_count_1="$(find "${db_dir}/oldgen" -maxdepth 1 -name '*.darc' | wc -l)"
    [ "$darc_count_1" -eq 1 ]

    # Delete the feature branch. Now every chunk reachable only from
    # feature is garbage. forceDelete bypasses the merged-into-main
    # safety check.
    run mongosh_eval "$db_name" '
        JSON.stringify(db.runCommand({doltBranch: 1, branch: "feature", forceDelete: true}))
    '
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1'

    # Second GC pass (full mode). Should reclaim the feature-only
    # chunks and shrink the store by 2x or more.
    run mongosh_eval "$db_name" 'JSON.stringify(db.runCommand({dumboGC: 1, mode: "full"}));'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .mode == "full"'
    local chunks_after_2 size_after_2
    chunks_after_2="$(echo "$output" | jq '.chunksAfter')"
    size_after_2="$(echo "$output" | jq '.sizeAfter')"

    # 2x reduction in both chunks and size vs. the first GC's
    # post-state. If the workload sizing or branch-delete cleanup
    # has regressed, this triggers. In practice the feature-heavy
    # workload sees 20x chunk reduction and 40x size reduction; the
    # 2x threshold leaves headroom for archive-format overhead and
    # main-baseline growth.
    [ "$(( chunks_after_2 * 2 ))" -le "$chunks_after_1" ]
    [ "$(( size_after_2 * 2 ))" -le "$size_after_1" ]

    # Integrity: store still passes dolt fsck after branch delete +
    # two GCs, and the feature branch is really gone (not just
    # cached as absent).
    stop_dumbodb
    setup_dolt_hack "$DUMBODB_DATA_DIR"
    cd "$(dirname "$DUMBODB_DATA_DIR")"

    run dolt fsck
    [ "$status" -eq 0 ]

    run dolt branch
    [ "$status" -eq 0 ]
    ! echo "$output" | grep -q "feature"
}
