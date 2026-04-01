# dongoCurrentBranch Verification

Manual verification guide for `dongoCurrentBranch` end-to-end behavior. Work through
each scenario top to bottom.

> **Automated equivalent:** `tests/versioning_current_branch_verify_test.go` (`TestCurrentBranchVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCurrentBranchVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with a branch

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("branchdb")
db.dropDatabase()

// Insert a document and commit (gives us a non-trivial history)
db.items.insertOne({ _id: 1, v: "first" })
const result1 = db.runCommand({ dongoCommit: 1, message: "first commit" })
printjson(result1)
// Expected: { hash: "<hash1>", branch: "main", message: "first commit", ok: 1 }
const hash1 = result1.hash

// Insert a second document and commit
db.items.insertOne({ _id: 2, v: "second" })
const result2 = db.runCommand({ dongoCommit: 1, message: "second commit" })
printjson(result2)
// Expected: { hash: "<hash2>", branch: "main", message: "second commit", ok: 1 }
const hash2 = result2.hash

// Create branch "feature" from main HEAD
db.getSiblingDB("branchdb__main").runCommand({ dongoBranch: 1, branch: "feature" })
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

No `__` suffix; defaults to the main branch.

```js
db.getSiblingDB("branchdb").runCommand({ dongoCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 2: `branchdb__main` — returns "main"

Explicit main branch rootish.

```js
db.getSiblingDB("branchdb__main").runCommand({ dongoCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 3: `branchdb__feature` — returns "feature"

Non-main branch rootish. Returns the branch name.

```js
db.getSiblingDB("branchdb__feature").runCommand({ dongoCurrentBranch: 1 })
// Expected: { branch: "feature", ok: 1 }
```

---

## Scenario 4: `branchdb__<hash>` — returns error (no branch name at a commit)

Commit hash rootish is read-only. There is no branch name to return.

```js
db.getSiblingDB("branchdb__" + hash1).runCommand({ dongoCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: dongoCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)
```

---

## Scenario 5: `branchdb__main~1` — returns error (no branch name at an ancestor)

Ancestor expression rootish is read-only. There is no branch name to return.

```js
db.getSiblingDB("branchdb__main~1").runCommand({ dongoCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: dongoCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)
```

---

## Quick Reference

| Connection | dongoCurrentBranch result |
|---|---|
| `mydb` (no suffix) | `{ branch: "main", ok: 1 }` |
| `mydb__main` | `{ branch: "main", ok: 1 }` |
| `mydb__feature` | `{ branch: "feature", ok: 1 }` |
| `mydb__<hash>` | OperationFailed (code 96) |
| `mydb__main~1` | OperationFailed (code 96) |

### Checking the error code in mongosh

```js
try {
  db.getSiblingDB("branchdb__" + hash1).runCommand({ dongoCurrentBranch: 1 })
} catch (e) {
  print("code:", e.code)      // 96
  print("message:", e.message)
}
```
