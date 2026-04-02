# dongoBranch Verification

Manual verification guide for `dongoBranch` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_branch_verify_test.go` (`TestBranchVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestBranchVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with two commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("branchvdb")
db.dropDatabase()

// Commit 1: one document
db.items.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ dongoCommit: 1, message: "commit one" })
printjson(r1)
// Expected: { hash: "<hash1>", branch: "main", message: "commit one", ok: 1 }
const hash1 = r1.hash

// Commit 2: second document added
db.items.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ dongoCommit: 1, message: "commit two" })
printjson(r2)
// Expected: { hash: "<hash2>", branch: "main", message: "commit two", ok: 1 }
const hash2 = r2.hash

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `branchvdb` has:
- **main** (HEAD = commit 2): two documents
- **hash1**: commit 1 (one document)
- **hash2**: commit 2 (same as current main HEAD)

---

## Scenario 1: Create branch from main HEAD — response shape

`dongoBranch` creates a new branch and returns `{ branch: "<name>", ok: 1 }`.
The connection must be a branch rootish (writable or hash — see Scenario 3).

```js
db.getSiblingDB("branchvdb__main").runCommand({ dongoBranch: 1, branch: "feature" })
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
`dongoDiff` between the two should show no changes.

```js
// Create "snapshot" branch from current main HEAD
db.getSiblingDB("branchvdb__main").runCommand({ dongoBranch: 1, branch: "snapshot" })

// Diff main vs snapshot — must be empty (identical commits)
db.getSiblingDB("branchvdb__main").runCommand({
  dongoDiff: 1,
  from: "snapshot",
  to:   "main"
})
// Expected: { "collections": [], "ok": 1 }
```

---

## Scenario 3: Branch isolation — writes on branch do not affect source

After branching, writes committed to the new branch are invisible from main, and
vice versa.

```js
var feature = db.getSiblingDB("branchvdb__feature")

// Add a document on the feature branch and commit it
feature.items.insertOne({ _id: 3, label: "gamma" })
feature.runCommand({ dongoCommit: 1, message: "feature adds gamma" })

// main must not see _id:3
db.getSiblingDB("branchvdb__main").items.countDocuments({})
// Expected: 2   (_id:3 is on feature only)

// feature must see all three documents
feature.items.countDocuments({})
// Expected: 3
```

---

## Scenario 4: Create branch from a commit hash rootish

`dongoBranch` works even when the connection is a read-only commit-hash rootish.
The new branch starts at that exact commit.

```js
// Create branch "at-commit-one" from the commit-hash rootish at hash1
db.getSiblingDB("branchvdb__" + hash1).runCommand({
  dongoBranch: 1,
  branch: "at-commit-one"
})
// Expected: { branch: "at-commit-one", ok: 1 }

// The new branch should see only the one document from commit 1
db.getSiblingDB("branchvdb__at-commit-one").items.find({}).toArray()
// Expected: [ { _id: 1, label: "alpha" } ]
```

Key check: `branch` is `"at-commit-one"`, `ok` is `1`; the branch has one document.

---

## Scenario 5: Create branch from an ancestor expression rootish

`dongoBranch` also works from an ancestor expression rootish (`branch~N`). The new
branch starts at the resolved ancestor commit. Use a fresh database to verify the
correct document count at the ancestor state.

```js
// Fresh database: two commits (hash1 = 1 doc, hash2/HEAD = 2 docs)
var db2 = db.getSiblingDB("branchvdb2")
db2.dropDatabase()
db2.items.insertOne({ _id: 1, label: "alpha" })
db2.runCommand({ dongoCommit: 1, message: "commit one" })
db2.items.insertOne({ _id: 2, label: "beta" })
db2.runCommand({ dongoCommit: 1, message: "commit two" })

// main~1 resolves to commit 1 (one document)
db2.getSiblingDB("branchvdb2__main~1").runCommand({
  dongoBranch: 1,
  branch: "back-one"
})
// Expected: { branch: "back-one", ok: 1 }

// back-one should see only one document (the state at main~1)
db2.getSiblingDB("branchvdb2__back-one").items.find({}).toArray()
// Expected: [ { _id: 1, label: "alpha" } ]
```

Key check: branch created successfully and sees only commit-1 state.

---

## Quick Reference

| Command | Connection | Result |
|---|---|---|
| `{ dongoBranch: 1, branch: "name" }` | `__main` | `{ branch: "name", ok: 1 }` |
| `{ dongoBranch: 1, branch: "name" }` | `__feature` | `{ branch: "name", ok: 1 }` |
| `{ dongoBranch: 1, branch: "name" }` | `__<hash>` | `{ branch: "name", ok: 1 }` |
| `{ dongoBranch: 1, branch: "name" }` | `__main~1` | `{ branch: "name", ok: 1 }` |

- `branch` in the response echoes the name you provided.
- Branch creation works from any rootish that resolves to a commit (branch name, hash, ancestor expression).
- The new branch HEAD equals the commit that was resolved from the source rootish.
- Writes on the new branch are isolated from the source branch.
