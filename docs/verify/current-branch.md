# doltCurrentBranch Verification

Manual verification guide for `doltCurrentBranch` end-to-end behavior. Work through
each scenario top to bottom.

> **Automated equivalent:** `tests/versioning_current_branch_verify_test.go` (`TestCurrentBranchVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCurrentBranchVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with a branch

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("branchdb")
db.dropDatabase()

// Insert a document and commit (gives us a non-trivial history)
db.items.insertOne({ _id: 1, v: "first" })
const result1 = db.runCommand({ doltCommit: 1, message: "first commit", author: "alice <alice@dumbodb>" })
printjson(result1)
// Expected: { hash: "<hash1>", branch: "main", message: "first commit", ok: 1 }
const hash1 = result1.commitId

// Insert a second document and commit
db.items.insertOne({ _id: 2, v: "second" })
const result2 = db.runCommand({ doltCommit: 1, message: "second commit", author: "alice <alice@dumbodb>" })
printjson(result2)
// Expected: { hash: "<hash2>", branch: "main", message: "second commit", ok: 1 }
const hash2 = result2.commitId

// Create branch "feature" from main HEAD
db.getSiblingDB("branchdb__d_main").runCommand({ doltBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `branchdb` has:
- **main** (HEAD = commit 2): two documents
- **feature**: branch pointing to commit 2 (same as main HEAD)
- **hash1**: commit 1 (one document)

---

## Scenario 1: Plain db name — returns "main"

No `__d_` suffix; defaults to the main branch.

```js
db.getSiblingDB("branchdb").runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 2: `branchdb__d_main` — returns "main"

Explicit main branch rootish.

```js
db.getSiblingDB("branchdb__d_main").runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 3: `branchdb__d_feature` — returns "feature"

Non-main branch rootish. Returns the branch name.

```js
db.getSiblingDB("branchdb__d_feature").runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "feature", ok: 1 }
```

---

## Scenario 4: `branchdb__d_<hash>` — returns error (no branch name at a commit)

Commit hash rootish is read-only. There is no branch name to return.

```js
db.getSiblingDB("branchdb__d_" + hash1).runCommand({ doltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)
```

---

## Scenario 5: `branchdb__d_main~1` — returns error (no branch name at an ancestor)

Ancestor expression rootish is read-only. There is no branch name to return.

```js
db.getSiblingDB("branchdb__d_main~1").runCommand({ doltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)
```

---

## Quick Reference

| Connection | doltCurrentBranch result |
|---|---|
| `mydb` (no suffix) | `{ branch: "main", ok: 1 }` |
| `mydb__d_main` | `{ branch: "main", ok: 1 }` |
| `mydb__d_feature` | `{ branch: "feature", ok: 1 }` |
| `mydb__d_<hash>` | OperationFailed (code 96) |
| `mydb__d_main~1` | OperationFailed (code 96) |

### Checking the error code in mongosh

```js
try {
  db.getSiblingDB("branchdb__d_" + hash1).runCommand({ doltCurrentBranch: 1 })
} catch (e) {
  print("code:", e.code)      // 96
  print("message:", e.message)
}
```
