#!/usr/bin/env bats
# End-to-end git-backed remote tests (dolt stored in a git repository) via dolt's
# GitRemoteFactory. Covers git+file (hermetic, git CLI only) and git+ssh against
# a REAL OpenSSH daemon. In CI sshd must be present (the workflow installs
# openssh-server); if it is missing there the git+ssh test FAILS rather than
# skipping. On a local box without sshd it falls back to a GIT_SSH_COMMAND
# wrapper so the git+ssh code path still runs.

load helpers

setup() {
    command -v git >/dev/null 2>&1 || { in_ci && { echo "ERROR: git required in CI" >&2; return 1; } || skip "git not installed"; }

    GIT_SSHD_WORK="$(mktemp -d)"
    if have_sshd; then
        start_git_sshd "$GIT_SSHD_WORK"
    elif in_ci; then
        echo "ERROR: sshd not found; CI must install openssh-server for git+ssh tests" >&2
        return 1
    else
        start_git_ssh_wrapper_only "$GIT_SSHD_WORK"
    fi

    # The dumbodb server shells out to git (and git to ssh); it needs both
    # GIT_SSH_COMMAND (exported by the transport setup above) and a git identity
    # for the commits the factory makes. Export before starting the server.
    export GIT_AUTHOR_NAME=dumbo GIT_AUTHOR_EMAIL=dumbo@test
    export GIT_COMMITTER_NAME=dumbo GIT_COMMITTER_EMAIL=dumbo@test

    DOLT_ISOLATED_HOME="$(mktemp -d)"
    export DOLT_ROOT_PATH="$DOLT_ISOLATED_HOME"

    DUMBODB_DATA_DIR="$(mktemp -d)"
    DUMBODB_PORT="$(free_port)"
    start_dumbodb "$DUMBODB_DATA_DIR" "$DUMBODB_PORT"

    GIT_REPOS_DIR="$(mktemp -d)"
}

teardown() {
    stop_dumbodb
    stop_git_sshd
    rm -rf "$DUMBODB_DATA_DIR" "$DOLT_ISOLATED_HOME" "$GIT_REPOS_DIR"
    unset DOLT_ROOT_PATH GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
}

# git_round_trip <remote-url>
# Seed+push+clone+fetch a git remote via the wire and assert the data survives.
git_round_trip() {
    local url="$1"
    local db_uri="mongodb://127.0.0.1:${DUMBODB_PORT}/test"

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
    echo "$output" | jq -e '.ok == 1'

    # Clone into a new database via admin and read the doc back.
    run mongo_json "mongodb://127.0.0.1:${DUMBODB_PORT}/admin" "db.runCommand({dumboClone:1,from:'${url}',as:'clonedb'})"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.ok == 1 and .db == "clonedb"'

    run mongo_json "mongodb://127.0.0.1:${DUMBODB_PORT}/clonedb" 'db.col.findOne({_id:1})'
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.name == "alice"'
}

@test 'git+file: dumboPush/dumboClone round trip' {
    local repo="${GIT_REPOS_DIR}/file-remote.git"
    seed_git_bare_repo "$repo" main
    git_round_trip "git+file://${repo}"
}

@test 'git+ssh: dumboPush/dumboClone round trip over a real sshd' {
    local repo="${GIT_REPOS_DIR}/ssh-remote.git"
    seed_git_bare_repo "$repo" main
    git_round_trip "$(git_ssh_url "$repo")"
}
