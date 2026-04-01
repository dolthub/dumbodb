# Rootish Connection String Verification

Manual verification guide for Dongo rootish connection string behavior. Work through each
scenario top to bottom. Each section builds on the previous setup, so run them in order.

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with two commits

Run this once before the verification scenarios below. It creates a database `verifydb`
with two commits so you have historical data to query.

```js
// Start fresh — drop if it exists
var db = db.getSiblingDB("verifydb")
db.dropDatabase()

// Insert a document and commit (commit 1)
db.items.insertOne({ _id: 1, label: "first", version: 1 })
const result1 = db.runCommand({ dongoCommit: 1, message: "first commit" })
printjson(result1)
// Expected:
// { hash: "<hash1>", branch: "main", message: "first commit", ok: 1 }
// Save hash1 for later:
const hash1 = result1.hash

// Insert a second document and commit (commit 2)
db.items.insertOne({ _id: 2, label: "second", version: 2 })
const result2 = db.runCommand({ dongoCommit: 1, message: "second commit" })
printjson(result2)
// Expected:
// { hash: "<hash2>", branch: "main", message: "second commit", ok: 1 }
const hash2 = result2.hash

// Create a branch named "v1.0" pointing at commit 2.
// The rootish in the db name must be percent-encoded because '.' is a
// MongoDB namespace separator. Encode client-side: "v1.0" → "v1%2E0".
// Dongo decodes it server-side before resolving the branch.
const tagResult = db.getSiblingDB("verifydb__main").runCommand({ dongoBranch: 1, branch: "v1.0" })
printjson(tagResult)
// Expected: { branch: "v1.0", ok: 1 }

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After running setup, `verifydb` has:
- **main** (HEAD = commit 2): two documents (`_id: 1` and `_id: 2`)
- **hash1**: snapshot with one document (`_id: 1` only)
- **hash2**: snapshot identical to current main
- **v1.0**: branch pointing to commit 2 (same as main HEAD); access via `verifydb__v1%2E0`

---

## Scenario 1: `verifydb__main` — reads and writes work

Branch rootish. Full read/write access.

```js
const main = db.getSiblingDB("verifydb__main")

// Read: should return both documents
main.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]

// Write: insert a third document (then remove it to keep state clean)
main.items.insertOne({ _id: 3, label: "third", version: 3 })
// Expected: { acknowledged: true, insertedId: 3 }

main.items.find({}).toArray()
// Expected: three documents

main.items.deleteOne({ _id: 3 })
// Expected: { acknowledged: true, deletedCount: 1 }

// dongoCurrentBranch works on branch rootish
main.runCommand({ dongoCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 2: `verifydb__v1%2E0` — reads and writes work, isolated from main

Non-main branch rootish. Full read/write access; writes go to that branch's working
set and are isolated from main's working set.

> **Percent-encoding:** Characters invalid in MongoDB database names (`.`, `/`, `$`, space)
> must be percent-encoded in the rootish part of the db name. Dongo decodes them
> server-side before resolving the branch. One pass only — `%` itself encodes as `%25`.
>
> Common encodings: `.` → `%2E`, `/` → `%2F`, `$` → `%24`

```js
const v1 = db.getSiblingDB("verifydb__v1%2E0")

// Read: should succeed and return both documents (same as main HEAD at setup time)
v1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]

// Write on v1.0 branch: inserts into the v1.0 working set, NOT main's working set
v1.items.insertOne({ _id: 10, label: "v1-only" })
// Expected: { acknowledged: true, insertedId: 10 }

// v1.0 now has three documents
v1.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... }, { _id: 10, label: "v1-only" } ]

// main is unchanged — the v1.0 write is isolated
const main = db.getSiblingDB("verifydb__main")
main.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]
// (_id:10 does NOT appear here)

// dongoCurrentBranch works on branch rootish
v1.runCommand({ dongoCurrentBranch: 1 })
// Expected: { branch: "v1.0", ok: 1 }

// Clean up the test write so subsequent scenarios start from a known state.
v1.items.deleteOne({ _id: 10 })
// Expected: { acknowledged: true, deletedCount: 1 }
```

---

## Scenario 3: `verifydb__<hash>` — connects to correct snapshot, reads correct historical data

Commit hash rootish. Read-only view of the exact snapshot at that commit.

```js
// Connect to the snapshot at hash1 (one document only)
const snap1 = db.getSiblingDB("verifydb__" + hash1)

snap1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (Only the document from commit 1 — _id:2 does not exist here)

snap1.items.countDocuments({})
// Expected: 1

// Connect to the snapshot at hash2 (two documents)
const snap2 = db.getSiblingDB("verifydb__" + hash2)

snap2.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... } ]

// Write on commit hash: must fail
snap1.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// dongoCurrentBranch is not available on commit hash rootish
snap1.runCommand({ dongoCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: dongoCurrentBranch: connection is read-only
//   (commit hash or ancestor expression); there is no current branch
```

---

## Scenario 4: `verifydb__main~1` — returns data as of parent commit, not current HEAD

Ancestor expression rootish. Read-only; resolves to the parent of the named branch's HEAD.

```js
const parent = db.getSiblingDB("verifydb__main~1")

// main~1 is the parent of main HEAD (commit 1: one document)
parent.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (_id:2 was added in commit 2, which is HEAD, not its parent)

parent.items.countDocuments({})
// Expected: 1

// main~0 is main itself (same as current HEAD)
const same = db.getSiblingDB("verifydb__main~0")
same.items.countDocuments({})
// Expected: 2

// Write on ancestor expression: must fail
parent.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// dongoCurrentBranch is not available on ancestor expression rootish
parent.runCommand({ dongoCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: dongoCurrentBranch: connection is read-only
//   (commit hash or ancestor expression); there is no current branch
```

---

## Scenario 5: `verifydb__HEAD` — returns a clear 'not supported' error

HEAD is not a valid rootish. Any operation on a `HEAD`-encoded database name fails immediately
at parse time — no query is executed.

```js
const head = db.getSiblingDB("verifydb__HEAD")

head.items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "HEAD": HEAD and HEAD-relative forms are not
//   supported; use a branch name, tag, commit hash, or <branch>~<N>

// Same for HEAD-relative forms
db.getSiblingDB("verifydb__HEAD~1").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "HEAD~1": HEAD and HEAD-relative forms are not
//   supported; use a branch name, tag, commit hash, or <branch>~<N>
```

---

## Scenario 6: `verifydb__main@{yesterday}` — returns a clear 'not supported' error

Reflog syntax is not supported. Any operation fails at parse time.

```js
db.getSiblingDB("verifydb__main@{yesterday}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{yesterday}": reflog syntax is not supported

// Other reflog forms also rejected
db.getSiblingDB("verifydb__main@{5 minutes ago}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{5 minutes ago}": reflog syntax is not supported

db.getSiblingDB("verifydb__@{1}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "@{1}": reflog syntax is not supported
```

---

## Scenario 7: `verifydb__main..feature` — returns a clear 'not supported' error

Range syntax is not supported. Any operation fails at parse time.

```js
db.getSiblingDB("verifydb__main..feature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main..feature": range syntax is not supported

// Three-dot range also rejected
db.getSiblingDB("verifydb__main...feature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main...feature": range syntax is not supported
```

---

## Quick Reference

| Rootish form | Example | Read | Write | Notes |
|---|---|---|---|---|
| Branch name (main) | `mydb__main` | ✅ | ✅ | Writes go to main's working set |
| Branch name (other) | `mydb__v1%2E0` (encodes `v1.0`) | ✅ | ✅ | Writes go to that branch's working set, isolated from main |
| Tag name | `mydb__v1%2E0` (when `v1.0` is a tag) | ✅ | ❌ | `cannot write to a read-only database snapshot` (code 96) |
| Commit hash (32 chars) | `mydb__<hash>` | ✅ | ❌ | `cannot write to a read-only database snapshot` (code 96) |
| Ancestor expression | `mydb__main~1` | ✅ | ❌ | `cannot write to a read-only database snapshot` (code 96) |
| HEAD | `mydb__HEAD` | ❌ | ❌ | `rootish "HEAD": HEAD and HEAD-relative forms are not supported...` (code 96) |
| Reflog | `mydb__main@{yesterday}` | ❌ | ❌ | `rootish "...": reflog syntax is not supported` (code 96) |
| Range | `mydb__main..feature` | ❌ | ❌ | `rootish "...": range syntax is not supported` (code 96) |

All errors use MongoDB error code **96** (`OperationFailed`).

### Checking the error code in mongosh

```js
try {
  db.getSiblingDB("verifydb__HEAD").items.find({}).toArray()
} catch (e) {
  print("code:", e.code)      // 96
  print("message:", e.message)
}
```
