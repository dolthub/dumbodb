# Rootish Connection String Verification

Manual verification guide for Docudolt rootish connection string behavior. Work through each
scenario top to bottom. Each section builds on the previous setup, so run them in order.

> **Automated equivalent:** `tests/versioning_rootish_verify_test.go` (`TestRootishVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestRootishVerify -v
> ```

## Prerequisites

A running Docudolt instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Docudolt address if different.

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
const result1 = db.runCommand({ docudoltCommit: 1, message: "first commit", author: "alice <alice@docudolt>" })
printjson(result1)
// Expected:
// { hash: "<hash1>", branch: "main", message: "first commit", ok: 1 }
// Save hash1 for later:
const hash1 = result1.commitId

// Insert a second document and commit (commit 2)
db.items.insertOne({ _id: 2, label: "second", version: 2 })
const result2 = db.runCommand({ docudoltCommit: 1, message: "second commit", author: "alice <alice@docudolt>" })
printjson(result2)
// Expected:
// { hash: "<hash2>", branch: "main", message: "second commit", ok: 1 }
const hash2 = result2.commitId

// Create a branch named "v1.0" pointing at commit 2.
// The rootish in the db name must be percent-encoded because '.' is a
// MongoDB namespace separator. Encode client-side: "v1.0" → "v1%2E0".
// Docudolt decodes it server-side before resolving the branch.
const tagResult = db.getSiblingDB("verifydb__d_main").runCommand({ docudoltBranch: 1, branch: "v1.0" })
printjson(tagResult)
// Expected: { branch: "v1.0", ok: 1 }

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After running setup, `verifydb` has:
- **main** (HEAD = commit 2): two documents (`_id: 1` and `_id: 2`)
- **hash1**: snapshot with one document (`_id: 1` only)
- **hash2**: snapshot identical to current main
- **v1.0**: branch pointing to commit 2 (same as main HEAD); access via `verifydb__d_v1%2E0`

---

## Scenario 1: `verifydb__d_main` — reads and writes work

Branch rootish. Full read/write access.

```js
const main = db.getSiblingDB("verifydb__d_main")

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

// docudoltCurrentBranch works on branch rootish
main.runCommand({ docudoltCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 2: `verifydb__d_v1%2E0` — reads and writes work, isolated from main

Non-main branch rootish. Full read/write access; writes go to that branch's working
set and are isolated from main's working set.

> **Percent-encoding:** Characters invalid in MongoDB database names (`.`, `/`, `$`, space)
> must be percent-encoded in the rootish part of the db name. Docudolt decodes them
> server-side before resolving the branch. One pass only — `%` itself encodes as `%25`.
>
> Common encodings: `.` → `%2E`, `/` → `%2F`, `$` → `%24`

```js
const v1 = db.getSiblingDB("verifydb__d_v1%2E0")

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
const main = db.getSiblingDB("verifydb__d_main")
main.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]
// (_id:10 does NOT appear here)

// docudoltCurrentBranch works on branch rootish
v1.runCommand({ docudoltCurrentBranch: 1 })
// Expected: { branch: "v1.0", ok: 1 }

// Clean up the test write so subsequent scenarios start from a known state.
v1.items.deleteOne({ _id: 10 })
// Expected: { acknowledged: true, deletedCount: 1 }
```

---

## Scenario 3: `verifydb__d_<hash>` — connects to correct snapshot, reads correct historical data

Commit hash rootish. Read-only view of the exact snapshot at that commit. Writes to
the collection are blocked, but branch creation works — creating a branch from a commit
hash is always valid because the hash is a fully resolved commit address.

```js
// Connect to the snapshot at hash1 (one document only)
const snap1 = db.getSiblingDB("verifydb__d_" + hash1)

snap1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (Only the document from commit 1 — _id:2 does not exist here)

snap1.items.countDocuments({})
// Expected: 1

// Connect to the snapshot at hash2 (two documents)
const snap2 = db.getSiblingDB("verifydb__d_" + hash2)

snap2.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... } ]

// Write on commit hash: must fail — the snapshot is read-only
snap1.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// docudoltCurrentBranch: no branch name to return (connection is at a specific commit)
snap1.runCommand({ docudoltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: docudoltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)

// docudoltBranch: works — branch creation only needs a resolved commit, not write access.
// This creates a new branch "from-hash1" pointing at hash1 (one-document state).
snap1.runCommand({ docudoltBranch: 1, branch: "from-hash1" })
// Expected: { branch: "from-hash1", ok: 1 }

// Verify the new branch sees the one-document state at hash1.
db.getSiblingDB("verifydb__d_from-hash1").items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
```

---

## Scenario 4: `verifydb__d_main~1` — returns data as of parent commit, not current HEAD

Ancestor expression rootish. Read-only; resolves to the parent of the named branch's HEAD.
As with commit hashes, writes to the collection are blocked but branch creation works.

```js
const parent = db.getSiblingDB("verifydb__d_main~1")

// main~1 is the parent of main HEAD (commit 1: one document)
parent.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (_id:2 was added in commit 2, which is HEAD, not its parent)

parent.items.countDocuments({})
// Expected: 1

// main~0 is main itself (same as current HEAD)
const same = db.getSiblingDB("verifydb__d_main~0")
same.items.countDocuments({})
// Expected: 2

// Write on ancestor expression: must fail — the snapshot is read-only
parent.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// docudoltCurrentBranch: no branch name to return (connection is at a specific commit)
parent.runCommand({ docudoltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: docudoltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)

// docudoltBranch: works — the ancestor expression resolves to a commit; branch creation
// only needs a resolved commit address. Creates "back-one" at the main~1 state.
parent.runCommand({ docudoltBranch: 1, branch: "back-one" })
// Expected: { branch: "back-one", ok: 1 }

// Verify back-one is at the one-document state (main~1).
db.getSiblingDB("verifydb__d_back-one").items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
```

---

## Scenario 5: `verifydb__d_HEAD` — returns a clear 'not supported' error

HEAD is not a valid rootish. The parse error is returned on the **first command** sent to
the server for that database name.

> **Why doesn't `getSiblingDB` itself fail?**
> `getSiblingDB()` is pure client-side JavaScript in mongosh — it constructs a local
> `Database` object and makes zero network calls. The server never sees the database
> name until a command is issued. There is no mechanism to validate earlier; the error
> fires on first contact, which is as early as the server can act.

```js
// getSiblingDB is client-side only — no server contact, no error yet.
const head = db.getSiblingDB("verifydb__d_HEAD")

// The parse error fires on the first command sent to the server:
head.items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "HEAD": HEAD and HEAD-relative forms are not
//   supported; use a branch name, tag, commit hash, or <branch>~<N>

// Any other command produces the same parse error:
head.runCommand({ ping: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "HEAD": HEAD and HEAD-relative forms are not
//   supported; use a branch name, tag, commit hash, or <branch>~<N>

// HEAD-relative forms are also rejected on first command:
db.getSiblingDB("verifydb__d_HEAD~1").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "HEAD~1": HEAD and HEAD-relative forms are not
//   supported; use a branch name, tag, commit hash, or <branch>~<N>
```

---

## Scenario 6: `verifydb__d_main@{yesterday}` — returns a clear 'not supported' error

Reflog syntax is not supported. The error fires on the first command (same reason as
Scenario 5 — `getSiblingDB` is client-side only).

```js
db.getSiblingDB("verifydb__d_main@{yesterday}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{yesterday}": reflog syntax is not supported

// Other reflog forms also rejected
db.getSiblingDB("verifydb__d_main@{5 minutes ago}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{5 minutes ago}": reflog syntax is not supported

db.getSiblingDB("verifydb__d_@{1}").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "@{1}": reflog syntax is not supported
```

---

## Scenario 7: `verifydb__d_main..feature` — returns a clear 'not supported' error

Range syntax is not supported. The error fires on the first command.

```js
db.getSiblingDB("verifydb__d_main..feature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main..feature": range syntax is not supported

// Three-dot range also rejected
db.getSiblingDB("verifydb__d_main...feature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main...feature": range syntax is not supported
```

---

## Quick Reference

| Rootish form | Example | Read | Write¹ | Branch creation² | Notes |
|---|---|---|---|---|---|
| Branch name (main) | `mydb__d_main` | ✅ | ✅ | ✅ | Writes go to main's working set |
| Branch name (other) | `mydb__d_v1%2E0` (encodes `v1.0`) | ✅ | ✅ | ✅ | Writes go to that branch's working set, isolated from main |
| Tag name | `mydb__d_v1%2E0` (when `v1.0` is a tag) | ✅ | ❌ | ✅ | Collection writes blocked; branch creation resolves the tag's commit |
| Commit hash (32 chars) | `mydb__d_<hash>` | ✅ | ❌ | ✅ | Collection writes blocked; branch creation uses the hash directly |
| Ancestor expression | `mydb__d_main~1` | ✅ | ❌ | ✅ | Collection writes blocked; branch creation walks to the Nth ancestor commit |
| HEAD | `mydb__d_HEAD` | ❌ | ❌ | ❌ | Rejected on first command (code 96); `getSiblingDB` is client-side only |
| Reflog | `mydb__d_main@{yesterday}` | ❌ | ❌ | ❌ | Rejected on first command (code 96) |
| Range | `mydb__d_main..feature` | ❌ | ❌ | ❌ | Rejected on first command (code 96) |

¹ **Write** = collection mutations (insertOne, updateOne, deleteOne, createCollection, etc.)
² **Branch creation** = `db.runCommand({ docudoltBranch: 1, branch: "newname" })`. Works whenever
the rootish resolves to a commit — branch creation only needs a commit address, not write access.

All errors use MongoDB error code **96** (`OperationFailed`).

### Checking the error code in mongosh

```js
try {
  db.getSiblingDB("verifydb__d_HEAD").items.find({}).toArray()
} catch (e) {
  print("code:", e.code)      // 96
  print("message:", e.message)
}
```
