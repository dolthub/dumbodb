# doltBranch Verification

Manual verification guide for `doltBranch` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/branch_test.go` (`TestBranchVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestBranchVerify -v
> ```

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
db.dropDatabase()

// Commit 1: one document
db.products.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ doltCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { hash: "<hash1>", branch: "main", message: "commit one", ok: 1 }
const hash1 = r1.commitId

// Commit 2: second document added
db.products.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ doltCommit: 1, message: "commit two", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { hash: "<hash2>", branch: "main", message: "commit two", ok: 1 }
const hash2 = r2.commitId

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `branchvdb` has:
- **main** (HEAD = commit 2): two documents
- **hash1**: commit 1 (one document)
- **hash2**: commit 2 (same as current main HEAD)

---

## Scenario 1: Create branch from main HEAD  -- response shape

`doltBranch` creates a new branch and returns `{ branch: "<name>", ok: 1 }`.
The connection must be a branch rootish (writable or hash  -- see Scenario 3).

```js
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "feature" })
```

Expected:

```json
{ "branch": "feature", "ok": 1 }
```

Key checks:
- `branch` echoes the name you provided
- `ok` is `1`

---

## Scenario 2: New branch points to the same commit as its source

Immediately after branching, the new branch HEAD equals the source branch HEAD.
`doltDiff` between the two should show no changes.

```js
// Create "snapshot" branch from current main HEAD
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "snapshot" })

// Diff main vs snapshot  -- must be empty (identical commits)
db.getSiblingDB("branchvdb@main").runCommand({
  doltDiff: 1,
  from: "snapshot",
  to:   "main"
})
// Expected: { "changes": [], "ok": 1 }
```

---

## Scenario 3: Branch isolation  -- writes on branch do not affect source

After branching, writes committed to the new branch are invisible from main, and
vice versa.

```js
var feature = db.getSiblingDB("branchvdb@feature")

// Add a document on the feature branch and commit it
feature.products.insertOne({ _id: 3, label: "gamma" })
feature.runCommand({ doltCommit: 1, message: "feature adds gamma", author: "carol <carol@startup.dev>" })

// main must not see _id:3
db.getSiblingDB("branchvdb@main").products.countDocuments({})
// Expected: 2   (_id:3 is on feature only)

// feature must see all three documents
feature.products.countDocuments({})
// Expected: 3
```

---

## Scenario 4: Create branch from a commit hash rootish

`doltBranch` works even when the connection is a read-only commit-hash rootish.
The new branch starts at that exact commit.

```js
// Create branch "at-commit-one" from the commit-hash rootish at hash1
db.getSiblingDB("branchvdb@" + hash1).runCommand({
  doltBranch: 1,
  branch: "at-commit-one"
})
// Expected: { branch: "at-commit-one", ok: 1 }

// The new branch should see only the one document from commit 1
db.getSiblingDB("branchvdb@at-commit-one").products.find({}).toArray()
// Expected: [ { _id: 1, label: "alpha" } ]
```

Key check: `branch` is `"at-commit-one"`, `ok` is `1`; the branch has one document.

---

## Scenario 5: Create branch from an ancestor expression rootish

`doltBranch` also works from an ancestor expression rootish (`branch~N`). The new
branch starts at the resolved ancestor commit. Use a fresh database to verify the
correct document count at the ancestor state.

```js
// Fresh database: two commits (hash1 = 1 doc, hash2/HEAD = 2 docs)
var db2 = db.getSiblingDB("branchvdb2")
db2.dropDatabase()
db2.products.insertOne({ _id: 1, label: "alpha" })
db2.runCommand({ doltCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
db2.products.insertOne({ _id: 2, label: "beta" })
db2.runCommand({ doltCommit: 1, message: "commit two", author: "bob <bob@widgets.io>" })

// main~1 resolves to commit 1 (one document)
db2.getSiblingDB("branchvdb2@main~1").runCommand({
  doltBranch: 1,
  branch: "back-one"
})
// Expected: { branch: "back-one", ok: 1 }

// back-one should see only one document (the state at main~1)
db2.getSiblingDB("branchvdb2@back-one").products.find({}).toArray()
// Expected: [ { _id: 1, label: "alpha" } ]
```

Key check: branch created successfully and sees only commit-1 state.

---

## Scenario 6: Safe delete (delete)  -- branch already merged into main

`doltBranch` with `delete: 1` deletes a branch only if its HEAD is reachable from
another branch (i.e. no data would be lost).  A branch whose HEAD equals the
source branch HEAD is always safe to delete.

```js
// Create a branch at current main HEAD and immediately safe-delete it.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "merged-branch" })
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "merged-branch", delete: 1 })
// Expected: { "branch": "merged-branch", "ok": 1 }
```

Key check: `branch` echoes the name, `ok` is `1`.

---

## Scenario 7: Safe delete (delete)  -- branch has unmerged commits, rejected

If the branch to delete has commits that are not reachable from any other branch,
safe delete must fail with an error.

```js
// Create "unmerged-branch" from main and add an exclusive commit.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "unmerged-branch" })
var ub = db.getSiblingDB("branchvdb@unmerged-branch")
ub.products.insertOne({ _id: 99, label: "extra" })
ub.runCommand({ doltCommit: 1, message: "extra commit", author: "carol <carol@startup.dev>" })

// Safe delete must fail.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "unmerged-branch", delete: 1 })
// Expected: error response  -- ok: 0, errmsg contains "unmerged commits"
```

Key check: command returns an error; `unmerged-branch` still exists afterwards.

---

## Scenario 8: Force delete (forceDelete)  -- branch has unmerged commits, succeeds

`doltBranch` with `forceDelete: 1` deletes a branch unconditionally, even if it has
commits that are not reachable from any other branch.

```js
// Create "force-branch" from main and add an exclusive commit.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "force-branch" })
var fb = db.getSiblingDB("branchvdb@force-branch")
fb.products.insertOne({ _id: 77, label: "gone" })
fb.runCommand({ doltCommit: 1, message: "unmerged commit", author: "bob <bob@widgets.io>" })

// Force delete succeeds regardless of merge status.
db.getSiblingDB("branchvdb@main").runCommand({ doltBranch: 1, branch: "force-branch", forceDelete: 1 })
// Expected: { "branch": "force-branch", "ok": 1 }
```

Key check: `branch` echoes the name, `ok` is `1`; the branch is gone.

---

## Scenario 9: Branch name that looks like a commit hash is rejected

Branch names must not end with a path segment that is exactly 32 lowercase
base32 characters (`[0-9a-v]{32}`), as this would be ambiguous with a Dolt
commit hash.

```js
// 32 lowercase base32 chars -- looks like a commit hash
db.runCommand({ doltBranch: 1, branch: "na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// Expected error: name looks like a commit hash

// Path ending with a hash-like segment is also rejected
db.runCommand({ doltBranch: 1, branch: "feature/na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// Expected error: name ends with a segment that looks like a commit hash

// Uppercase is fine -- commit hashes are always lowercase
db.runCommand({ doltBranch: 1, branch: "NA7KFRA98H45FR2U5QTR30O2GGM7VH61" })
// Expected: { branch: "NA7KFRA98H45FR2U5QTR30O2GGM7VH61", ok: 1 }

// Path-like branch names work
db.runCommand({ doltBranch: 1, branch: "team/alice/experiment" })
// Expected: { branch: "team/alice/experiment", ok: 1 }

// Hash-like segment in the middle is allowed -- only the last segment matters
db.runCommand({ doltBranch: 1, branch: "na7kfra98h45fr2u5qtr30o2ggm7vh61/feature" })
// Expected: { branch: "na7kfra98h45fr2u5qtr30o2ggm7vh61/feature", ok: 1 }
```

Key checks:
- Bare 32-char lowercase base32 name is rejected
- Path ending with such a segment is rejected
- Uppercase 32-char name is allowed (not a valid commit hash)
- Path-like branch names work
- Hash-like segment in the middle is allowed

---

## Scenario 10: List branches  -- no `branch` argument

Omitting `branch` lists every branch in the database, as `git branch` does.
Entries are sorted by name and the branch encoded in the connection is flagged
with `current: true`.

Use a fresh database so the listing is exact:

```js
var lb = db.getSiblingDB("branchlistdb")
lb.dropDatabase()
lb.products.insertOne({ _id: 1, label: "alpha" })
lb.runCommand({ doltCommit: 1, message: "commit one" })

// Created out of alphabetical order to show the listing is sorted.
lb.runCommand({ doltBranch: 1, branch: "zeta" })
lb.runCommand({ doltBranch: 1, branch: "alpha" })

db.getSiblingDB("branchlistdb@main").runCommand({ doltBranch: 1 })
```

Expected:

```json
{
  "branches": [
    { "name": "alpha", "commitId": "<hash>", "current": false },
    { "name": "main",  "commitId": "<hash>", "current": true  },
    { "name": "zeta",  "commitId": "<hash>", "current": false }
  ],
  "ok": 1
}
```

Key checks:
- Every branch appears exactly once, sorted by `name`
- `commitId` is the branch HEAD commit; `alpha` and `zeta` branched from `main`
  HEAD, so all three match here
- Exactly one entry has `current: true`  -- the branch encoded in the connection

Listing follows the connection, not the default branch:

```js
db.getSiblingDB("branchlistdb@zeta").runCommand({ doltBranch: 1 })
// Expected: same three entries, but "zeta" is the one with current: true
```

A connection pinned to a commit hash or an ancestor expression is not on any
branch, so no entry is current:

```js
const h = db.getSiblingDB("branchlistdb@main").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
db.getSiblingDB("branchlistdb@" + h).runCommand({ doltBranch: 1 })
// Expected: the same three entries, all with current: false
```

---

## Scenario 11: Listing does not swallow a malformed request

Only an absent `branch` lists. An explicit empty string is still an error, so a
client that builds the name from a variable cannot silently list every branch
when that variable is empty.

```js
db.getSiblingDB("branchlistdb@main").runCommand({ doltBranch: 1, branch: "" })
// Expected error: BadValue: dumboBranch: branch name must not be empty
```

Deleting still requires a name:

```js
db.getSiblingDB("branchlistdb@main").runCommand({ doltBranch: 1, delete: 1 })
// Expected error: BadValue: dumboBranch: branch name is required for delete

db.getSiblingDB("branchlistdb@main").runCommand({ doltBranch: 1, forceDelete: 1 })
// Expected error: BadValue: dumboBranch: branch name is required for delete
```

Key checks:
- `branch: ""` is rejected, not treated as a list request
- `delete` / `forceDelete` without `branch` is rejected

---

## Scenario 12: List remote-tracking branches (`git branch -r` / `-a`)

A plain listing shows local branches. `remote: true` shows the remote-tracking
branches -- the `refs/remotes/<remote>/<branch>` refs written by `dumboClone` /
`dumboFetch` / `dumboPush` -- and `all: true` shows both.

Set up a database with two branches and push them to a `file://` remote (push
writes the tracking refs into the local store). Substitute an empty/nonexistent
path for `<RT_REMOTE_DIR>`.

```js
var rt = db.getSiblingDB("rtlistdb")
rt.dropDatabase()
rt.items.insertOne({ _id: 1 })
rt.runCommand({ doltCommit: 1, message: "c1" })
rt.runCommand({ doltBranch: 1, branch: "feature" })

rt.runCommand({ doltRemote: 1, action: "add", name: "origin", url: "file://<RT_REMOTE_DIR>" })
db.getSiblingDB("rtlistdb@main").runCommand({ doltPush: 1, to: "origin", refSpec: "main" })
db.getSiblingDB("rtlistdb@feature").runCommand({ doltPush: 1, to: "origin", refSpec: "feature" })
```

A plain listing is local-only:

```js
db.getSiblingDB("rtlistdb@main").runCommand({ doltBranch: 1 })
// Expected: entries "feature" and "main"; no remoteTracking marker.
```

`remote: true` lists the tracking branches (`git branch -r`):

```js
db.getSiblingDB("rtlistdb@main").runCommand({ doltBranch: 1, remote: true })
```

```json
{
  "branches": [
    { "name": "origin/feature", "commitId": "<hash>", "current": false,
      "remoteTracking": true, "remote": "origin", "ref": "feature" },
    { "name": "origin/main", "commitId": "<hash>", "current": false,
      "remoteTracking": true, "remote": "origin", "ref": "main" }
  ],
  "ok": 1
}
```

`all: true` lists both (`git branch -a`):

```js
db.getSiblingDB("rtlistdb@main").runCommand({ doltBranch: 1, all: true })
// Expected: "feature", "main", "origin/feature", "origin/main".
```

Key checks:
- A remote-tracking entry's `name` is `<remote>/<branch>`, and it carries
  `remoteTracking: true`, `remote`, and `ref`
- A remote-tracking branch is never `current` and has no `upstream`
- `remote` / `all` are listing-only; passing them with a `branch` name errors

---

## Quick Reference

| Command | Connection | Result |
|---|---|---|
| `{ doltBranch: 1, branch: "name" }` | `@main` | `{ branch: "name", ok: 1 }` |
| `{ doltBranch: 1, branch: "name" }` | `@feature` | `{ branch: "name", ok: 1 }` |
| `{ doltBranch: 1, branch: "name" }` | `@<hash>` | `{ branch: "name", ok: 1 }` |
| `{ doltBranch: 1, branch: "name" }` | `@main~1` | `{ branch: "name", ok: 1 }` |
| `{ doltBranch: 1, branch: "name", delete: 1 }` | `@main` | `{ branch: "name", ok: 1 }` (merged) or error (unmerged) |
| `{ doltBranch: 1, branch: "name", forceDelete: 1 }` | `@main` | `{ branch: "name", ok: 1 }` (always) |
| `{ doltBranch: 1 }` | `@main` | `{ branches: [ { name, commitId, current }, ... ], ok: 1 }` |
| `{ doltBranch: 1 }` | `@<hash>` | same list, every entry `current: false` |
| `{ doltBranch: 1, remote: true }` | `@main` | remote-tracking branches (`git branch -r`) |
| `{ doltBranch: 1, all: true }` | `@main` | local + remote-tracking branches (`git branch -a`) |

- `branch` in the response echoes the name you provided.
- Branch creation works from any rootish that resolves to a commit (branch name, hash, ancestor expression).
- The new branch HEAD equals the commit that was resolved from the source rootish.
- Writes on the new branch are isolated from the source branch.
- `delete: 1` (safe delete): fails if the branch has commits not reachable from any other branch.
- `forceDelete: 1` (force delete): succeeds unconditionally.
- `delete` and `forceDelete` are mutually exclusive; passing both returns an error.
- Omitting `branch` lists every branch, sorted by name, with the connection's branch flagged `current: true`.
- An explicit `branch: ""` is an error, not a list request; `delete`/`forceDelete` still require a name.
- `remote: true` lists remote-tracking branches (`git branch -r`) and `all: true` lists local plus remote-tracking (`git branch -a`); a remote-tracking entry is `<remote>/<branch>` with `remoteTracking: true`, never `current`, and carries no `upstream`.
