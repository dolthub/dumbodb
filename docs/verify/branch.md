# doltBranch Verification

Manual verification guide for `doltBranch` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/branch_test.go` (`TestBranchVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestBranchVerify -v
> ```

`doltBranch` takes a required `action`, like `doltRemote`:

| action   | Fields                              | Meaning                                              |
|----------|-------------------------------------|------------------------------------------------------|
| `add`    | `branch`, optional `setConfig`      | Create `branch` from the connection rootish          |
| `update` | `branch`, `setConfig`               | Change `branch`'s `config.{pull,push}`               |
| `remove` | `branch`, optional `force`          | Delete `branch` (`force: true` skips the safety check)|
| `list`   | (none)                              | List every branch (local and remote-tracking)        |

`setConfig` is the single config write surface: a value **sets** a leaf; `null`
**clears** it, or clears a whole `pull`/`push` sub-object. See Scenario 13.

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with two commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("branchvdb")

// Commit 1: one document
db.products.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ doltCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
const hash1 = r1.commitId

// Commit 2: second document added
db.products.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ doltCommit: 1, message: "commit two", author: "bob <bob@widgets.io>" })
const hash2 = r2.commitId

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `branchvdb` has:
- **main** (HEAD = commit 2): two documents
- **hash1**: commit 1 (one document)
- **hash2**: commit 2 (same as current main HEAD)

---

## Scenario 1: Add a branch from main HEAD -- response shape

`action: "add"` creates a new branch and returns `{ branch: "<name>", ok: 1 }`.

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "feature" })
```

Expected:

```json
{ "branch": "feature", "ok": 1 }
```

---

## Scenario 2: New branch points to the same commit as its source

A new branch starts at the connection rootish's commit, so a diff against the
source is empty.

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "snapshot" })
db.getSiblingDB("branchvdb@main").runCommand({ doltDiff: 1, from: "snapshot", to: "main" })
// Expected: no collection changes -- identical commits.
```

---

## Scenario 3: Branch isolation -- writes on a branch do not affect the source

```js
var f = db.getSiblingDB("branchvdb@feature")
f.products.insertOne({ _id: 3, label: "gamma" })
f.runCommand({ doltCommit: 1, message: "feature adds gamma", author: "alice <alice@acme.com>" })

db.getSiblingDB("branchvdb@main").products.countDocuments({})    // 2 -- unchanged
f.products.countDocuments({})                                    // 3
```

---

## Scenario 4: Add a branch from a commit hash rootish

```js
db.getSiblingDB("branchvdb@" + hash1).runCommand({ doltBranch: 1, action: "add", branch: "at-commit-one" })
db.getSiblingDB("branchvdb@at-commit-one").products.countDocuments({})   // 1 (commit 1 state)
```

---

## Scenario 5: Add a branch from an ancestor expression rootish

`main~1` resolves to commit 1.

```js
db.getSiblingDB("branchvdb@main~1").runCommand({ doltBranch: 1, action: "add", branch: "back-one" })
db.getSiblingDB("branchvdb@back-one").products.countDocuments({})   // 1
```

---

## Scenario 6: Safe remove -- branch already merged into main

`action: "remove"` with no `force` is a safe delete: it refuses if the branch has
commits not reachable from any other branch. A branch at main's HEAD is reachable,
so it removes cleanly.

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "merged-branch" })
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "remove", branch: "merged-branch" })
// Expected: { branch: "merged-branch", ok: 1 }
```

---

## Scenario 7: Safe remove -- branch has unmerged commits, rejected

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "unmerged-branch" })
var u = db.getSiblingDB("branchvdb@unmerged-branch")
u.products.insertOne({ _id: 99, label: "extra" })
u.runCommand({ doltCommit: 1, message: "extra commit", author: "alice <alice@acme.com>" })

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "remove", branch: "unmerged-branch" })
// Expected: ok: 0 -- has unmerged commits; use force: true.
```

---

## Scenario 8: Force remove -- branch has unmerged commits, succeeds

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "remove", branch: "unmerged-branch", force: true })
// Expected: { branch: "unmerged-branch", ok: 1 }
```

---

## Scenario 9: A branch name that looks like a commit hash is rejected

A 32-char lowercase base32 name (the last path segment) collides with the commit
rootish namespace and is refused.

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// Expected: ok: 0.

// Only the last segment matters; a hash-like middle segment or uppercase is fine.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "team/alice/experiment" })   // ok
```

---

## Scenario 10: List branches (`action: "list"`)

`action: "list"` returns every branch, sorted by name, with the connection's
branch flagged `current: true`.

```js
var l = db.getSiblingDB("branchvdb@main")
l.runCommand({ doltBranch: 1, action: "add", branch: "zeta" })
l.runCommand({ doltBranch: 1, action: "add", branch: "alpha" })
l.runCommand({ doltBranch: 1, action: "list" })
```

Expected -- sorted, with `current` on the connection branch:

```json
{
  "branches": [
    { "name": "alpha",   "commitId": "<hash>" },
    { "name": "feature", "commitId": "<hash>" },
    { "name": "main",    "commitId": "<hash>", "current": true },
    { "name": "snapshot","commitId": "<hash>" },
    { "name": "zeta",    "commitId": "<hash>" }
  ],
  "ok": 1
}
```

`current` follows the connection, not the default branch: listing on
`branchvdb@zeta` marks `zeta` current. A hash rootish is on no branch, so nothing
is `current`.

---

## Scenario 11: `action` is required and per-action fields are validated

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1 })
// Expected: ok: 0 -- action is required (add, update, remove, list).

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "rename" })
// Expected: ok: 0 -- unknown action.

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "add", branch: "" })
// Expected: ok: 0 -- branch name must not be empty.

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "list", branch: "zeta" })
// Expected: ok: 0 -- "branch" is not valid with action "list".

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "remove" })
// Expected: ok: 0 -- branch name must not be empty.

db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main" })
// Expected: ok: 0 -- action update requires setConfig.
```

---

## Scenario 12: A listing includes remote-tracking branches with tracking info

A listing shows local branches (with their `config`) **and** remote-tracking
branches (`refs/remotes/<remote>/<branch>`). Set up a database whose `main`
tracks a remote at `/tmp/dumbo-rt-remote` (remove it first for a clean run).

```js
var rt = db.getSiblingDB("rtlistdb")
rt.items.insertOne({ _id: 1 })
rt.runCommand({ doltCommit: 1, message: "c1" })
rt.runCommand({ doltBranch: 1, action: "add", branch: "feature" })

rt.runCommand({ doltRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-rt-remote" })
db.getSiblingDB("rtlistdb@main").runCommand({ doltPush: 1, to: "origin", refSpec: "main" })
db.getSiblingDB("rtlistdb@feature").runCommand({ doltPush: 1, to: "origin", refSpec: "feature" })
db.getSiblingDB("rtlistdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main", setConfig: { pull: { remote: "origin", branch: "main" } } })

db.getSiblingDB("rtlistdb@main").runCommand({ doltBranch: 1, action: "list" })
```

Expected -- local branches (`main` carries its `config.pull`) alongside the
`origin/*` remote-tracking entries:

```json
{
  "branches": [
    { "name": "feature", "commitId": "<hash>" },
    { "name": "main", "commitId": "<hash>", "current": true,
      "config": { "pull": { "remote": "origin", "branch": "main" } } },
    { "name": "origin/feature", "commitId": "<hash>",
      "remoteTracking": true, "remote": "origin", "ref": "feature" },
    { "name": "origin/main", "commitId": "<hash>",
      "remoteTracking": true, "remote": "origin", "ref": "main" }
  ],
  "ok": 1
}
```

A remote-tracking entry carries `remoteTracking: true`, `remote`, and `ref`, is
never `current`, and has no `config`.

---

## Scenario 13: Branch config -- a value sets, `null` clears

`config` groups by direction: `config.pull` `{ remote, branch, rebase, ff }` is
the fetch upstream and pull policy; `config.push` `{ remote, branch }` is a
persistent push target. Set leaves with `action: "update"` and `setConfig`; clear
a leaf or a whole sub-object by setting it to `null`. Use a database whose `main`
tracks a remote at `/tmp/dumbo-cfg-remote` (remove it first).

```js
var c = db.getSiblingDB("cfgdb")
c.items.insertOne({ _id: 1 })
c.runCommand({ doltCommit: 1, message: "c1" })
c.runCommand({ doltBranch: 1, action: "add", branch: "feature" })
c.runCommand({ doltRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-cfg-remote" })
db.getSiblingDB("cfgdb@main").runCommand({ doltPush: 1, to: "origin", refSpec: "main" })

// Set config.pull identity + policy in one call.
db.getSiblingDB("cfgdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main", setConfig: { pull: { remote: "origin", branch: "main", rebase: true, ff: "only" } } })
// { branch: "main", config: { pull: { remote: "origin", branch: "main", rebase: "true", ff: "only" } }, ok: 1 }

// Add a config.push triangular target; pull is untouched.
db.getSiblingDB("cfgdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main", setConfig: { push: { remote: "origin", branch: "rev51" } } })

// null clears a single leaf.
db.getSiblingDB("cfgdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main", setConfig: { pull: { rebase: null } } })
// config.pull keeps remote/branch/ff; rebase is gone.

// null on a whole sub-object clears it.
db.getSiblingDB("cfgdb@main").runCommand({ doltBranch: 1, action: "update", branch: "main", setConfig: { push: null } })
// config.push is gone.
```

Validation (all rejected with `ok: 0`):
- `rebase`/`ff` without a `config.pull` upstream (e.g. on `feature`)
- a partial `config.push` (`{ remote }` without `branch`)
- an unknown remote name
- `rebase` that is not a bool; `ff` other than `"no"`/`"only"` (there is no `"default"` -- use `null`)
- an unknown key (`pull.squash`, a top-level key other than `pull`/`push`); a non-document, non-null sub-object; an empty `setConfig`

`rebase: false` clears `rebase` (the same as `null`), since dumbo has no distinct
"explicit false" pull behavior.

---

## Scenario 14: Removing a branch clears its config

A branch's `config` is part of the branch: removing it drops the config, so a
branch recreated under the same name starts clean and a bare push does not follow
the deleted branch's push target. Use a database with two remotes at
`/tmp/dumbo-del-1` and `/tmp/dumbo-del-2` (remove them first).

```js
var d = db.getSiblingDB("delcfgdb")
d.items.insertOne({ _id: 1 })
d.runCommand({ doltCommit: 1, message: "c1" })
d.runCommand({ doltRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-del-1" })
d.runCommand({ doltRemote: 1, action: "add", name: "origin2", url: "file:///tmp/dumbo-del-2" })

db.getSiblingDB("delcfgdb@main").runCommand({ doltBranch: 1, action: "add", branch: "release" })
db.getSiblingDB("delcfgdb@release").runCommand({ doltBranch: 1, action: "update", branch: "release", setConfig: {
  pull: { remote: "origin", branch: "main" },
  push: { remote: "origin2", branch: "release" }
} })

// Remove and recreate release.
db.getSiblingDB("delcfgdb@main").runCommand({ doltBranch: 1, action: "remove", branch: "release", force: true })
db.getSiblingDB("delcfgdb@main").runCommand({ doltBranch: 1, action: "add", branch: "release" })

db.getSiblingDB("delcfgdb@main").runCommand({ doltBranch: 1, action: "list" })
// Expected: the "release" entry has no "config" field.

db.getSiblingDB("delcfgdb@release").runCommand({ dumboPush: 1 })
// Expected: ok: 0 -- release has no push or pull config.
```

---

## Scenario 15: `action: "add"` with `setConfig` applies config atomically

`add` accepts a `setConfig`: the branch is created and the config applied in one
call. If the config is invalid, the branch is rolled back (add is all-or-nothing).

```js
var a = db.getSiblingDB("addcfgdb")
a.items.insertOne({ _id: 1 })
a.runCommand({ doltCommit: 1, message: "c1" })
a.runCommand({ doltRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-add-remote" })

db.getSiblingDB("addcfgdb@main").runCommand({ doltBranch: 1, action: "add", branch: "feature", setConfig: { pull: { remote: "origin", branch: "main" } } })
// { branch: "feature", config: { pull: { remote: "origin", branch: "main" } }, ok: 1 }

db.getSiblingDB("addcfgdb@main").runCommand({ doltBranch: 1, action: "add", branch: "bad", setConfig: { pull: { rebase: true } } })
// Expected: ok: 0 -- rebase needs an upstream; "bad" is NOT created.
```

---

## Quick Reference

| Command | Connection | Result |
|---|---|---|
| `{ doltBranch: 1, action: "add", branch: "x" }` | `@main` | `{ branch: "x", ok: 1 }` |
| `{ doltBranch: 1, action: "add", branch: "x", setConfig: {...} }` | `@main` | `{ branch: "x", config: {...}, ok: 1 }` (atomic) |
| `{ doltBranch: 1, action: "update", branch: "x", setConfig: {...} }` | `@main` | `{ branch: "x", config: {...}, ok: 1 }` |
| `{ doltBranch: 1, action: "remove", branch: "x" }` | `@main` | `{ branch: "x", ok: 1 }` (merged) or `ok: 0` (unmerged) |
| `{ doltBranch: 1, action: "remove", branch: "x", force: true }` | `@main` | `{ branch: "x", ok: 1 }` (always) |
| `{ doltBranch: 1, action: "list" }` | `@main` | `{ branches: [ { name, commitId, current?, config?, remoteTracking? }, ... ], ok: 1 }` |

- `action` is required: `add`, `update`, `remove`, or `list`. Missing or unknown is an error.
- `add` creates from the connection rootish (branch name, hash, or `branch~N`); the new HEAD equals the resolved commit; data is isolated from the source.
- `remove` is a safe delete unless `force: true`; a safe delete refuses a branch with commits not reachable elsewhere.
- `setConfig` (on `add` or `update`) sets `config.pull` `{ remote, branch, rebase, ff }` and/or `config.push` `{ remote, branch }`. A value sets a leaf; `null` clears it (or a whole `pull`/`push` sub-object). `rebase`/`ff` require a `config.pull` upstream; `config.push` is complete (both `remote` and `branch`) or absent.
- A listing includes local branches (with their `config` when set) and remote-tracking branches (`<remote>/<branch>`, `remoteTracking: true`, never `current`, no `config`).
