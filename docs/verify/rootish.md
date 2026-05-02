# Rootish Connection String Verification

Manual verification guide for DumboDB rootish connection string behavior. Work through each
scenario top to bottom. Each section builds on the previous setup, so run them in order.

> **Automated equivalent:** `tests/versioning_rootish_verify_test.go` (`TestRootishVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestRootishVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with two commits

Run this once before the verification scenarios below. It creates a database `verifydb`
with two commits so you have historical data to query.

```js
// Start fresh  -- drop if it exists
var db = db.getSiblingDB("verifydb")
db.dropDatabase()

// Insert a document and commit (commit 1)
db.items.insertOne({ _id: 1, label: "first", version: 1 })
const result1 = db.runCommand({ doltCommit: 1, message: "first commit", author: "alice <alice@acme.com>" })
printjson(result1)
// Expected:
// { hash: "<hash1>", branch: "main", message: "first commit", ok: 1 }
// Save hash1 for later:
const hash1 = result1.commitId

// Insert a second document and commit (commit 2)
db.items.insertOne({ _id: 2, label: "second", version: 2 })
const result2 = db.runCommand({ doltCommit: 1, message: "second commit", author: "bob <bob@widgets.io>" })
printjson(result2)
// Expected:
// { hash: "<hash2>", branch: "main", message: "second commit", ok: 1 }
const hash2 = result2.commitId

// Create a branch named "v1.0" pointing at commit 2.
// The rootish in the db name must be percent-encoded because '.' is a
// MongoDB namespace separator. Encode client-side: "v1.0" -> "v1%2E0".
// DumboDB decodes it server-side before resolving the branch.
const tagResult = db.getSiblingDB("verifydb@main").runCommand({ doltBranch: 1, branch: "v1.0" })
printjson(tagResult)
// Expected: { branch: "v1.0", ok: 1 }

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After running setup, `verifydb` has:
- **main** (HEAD = commit 2): two documents (`_id: 1` and `_id: 2`)
- **hash1**: snapshot with one document (`_id: 1` only)
- **hash2**: snapshot identical to current main
- **v1.0**: branch pointing to commit 2 (same as main HEAD); access via `verifydb@v1%2E0`

---

## Scenario 1: `verifydb@main`  -- reads and writes work

Branch rootish. Full read/write access.

```js
const main = db.getSiblingDB("verifydb@main")

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

// doltCurrentBranch works on branch rootish
main.runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }
```

---

## Scenario 2: `verifydb@v1%2E0`  -- reads and writes work, isolated from main

Non-main branch rootish. Full read/write access; writes go to that branch's working
set and are isolated from main's working set.

> **Percent-encoding:** Characters invalid in MongoDB database names (`.`, `/`, `$`, space)
> must be percent-encoded in the rootish part of the db name. DumboDB decodes them
> server-side before resolving the branch. One pass only  -- `%` itself encodes as `%25`.
>
> Common encodings: `.` -> `%2E`, `/` -> `%2F`, `$` -> `%24`

```js
const v1 = db.getSiblingDB("verifydb@v1%2E0")

// Read: should succeed and return both documents (same as main HEAD at setup time)
v1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]

// Write on v1.0 branch: inserts into the v1.0 working set, NOT main's working set
v1.items.insertOne({ _id: 10, label: "v1-only" })
// Expected: { acknowledged: true, insertedId: 10 }

// v1.0 now has three documents
v1.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... }, { _id: 10, label: "v1-only" } ]

// main is unchanged  -- the v1.0 write is isolated
const main = db.getSiblingDB("verifydb@main")
main.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]
// (_id:10 does NOT appear here)

// doltCurrentBranch works on branch rootish
v1.runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "v1.0", ok: 1 }

// Clean up the test write so subsequent scenarios start from a known state.
v1.items.deleteOne({ _id: 10 })
// Expected: { acknowledged: true, deletedCount: 1 }
```

---

## Scenario 3: `verifydb@<hash>`  -- connects to correct snapshot, reads correct historical data

Commit hash rootish. Read-only view of the exact snapshot at that commit. Writes to
the collection are blocked, but branch creation works  -- creating a branch from a commit
hash is always valid because the hash is a fully resolved commit address.

```js
// Connect to the snapshot at hash1 (one document only)
const snap1 = db.getSiblingDB("verifydb@" + hash1)

snap1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (Only the document from commit 1  -- _id:2 does not exist here)

snap1.items.countDocuments({})
// Expected: 1

// Connect to the snapshot at hash2 (two documents)
const snap2 = db.getSiblingDB("verifydb@" + hash2)

snap2.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... } ]

// Write on commit hash: must fail  -- the snapshot is read-only
snap1.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// doltCurrentBranch: no branch name to return (connection is at a specific commit)
snap1.runCommand({ doltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)

// doltBranch: works  -- branch creation only needs a resolved commit, not write access.
// This creates a new branch "from-hash1" pointing at hash1 (one-document state).
snap1.runCommand({ doltBranch: 1, branch: "from-hash1" })
// Expected: { branch: "from-hash1", ok: 1 }

// Verify the new branch sees the one-document state at hash1.
db.getSiblingDB("verifydb@from-hash1").items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
```

---

## Scenario 4: `verifydb@main~1`  -- returns data as of parent commit, not current HEAD

Ancestor expression rootish. Read-only; resolves to the parent of the named branch's HEAD.
As with commit hashes, writes to the collection are blocked but branch creation works.

```js
const parent = db.getSiblingDB("verifydb@main~1")

// main~1 is the parent of main HEAD (commit 1: one document)
parent.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (_id:2 was added in commit 2, which is HEAD, not its parent)

parent.items.countDocuments({})
// Expected: 1

// main~0 is main itself (same as current HEAD)
const same = db.getSiblingDB("verifydb@main~0")
same.items.countDocuments({})
// Expected: 2

// Write on ancestor expression: must fail  -- the snapshot is read-only
parent.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// doltCurrentBranch: no branch name to return (connection is at a specific commit)
parent.runCommand({ doltCurrentBranch: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltCurrentBranch: no current branch name
//   (connection is at a specific commit, not a named branch)

// doltBranch: works  -- the ancestor expression resolves to a commit; branch creation
// only needs a resolved commit address. Creates "back-one" at the main~1 state.
parent.runCommand({ doltBranch: 1, branch: "back-one" })
// Expected: { branch: "back-one", ok: 1 }

// Verify back-one is at the one-document state (main~1).
db.getSiblingDB("verifydb@back-one").items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
```

---

## Scenario 5: `verifydb@HEAD`  -- aliases the default branch (main)

HEAD is a rootish alias for the default branch. DumboDB connections are stateless, so there
is no per-session "current branch" the way `git` has a per-working-tree HEAD. Since the only
default branch DumboDB knows is `main`, HEAD is rewritten to `main` before resolution.

Concretely: `verifydb@HEAD` behaves identically to `verifydb@main` (reads the working
set, writes go to main's working set). `verifydb@HEAD~N` behaves identically to
`verifydb@main~N` (read-only snapshot of the Nth first-parent ancestor).

```js
const head = db.getSiblingDB("verifydb@HEAD")

// Read: returns both documents, same as connecting via @main.
head.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]

// doltCurrentBranch resolves HEAD to the default branch and returns "main".
head.runCommand({ doltCurrentBranch: 1 })
// Expected: { branch: "main", ok: 1 }

// Write via HEAD goes to main's working set. Insert then remove to keep state clean.
head.items.insertOne({ _id: 5, label: "via-HEAD" })
// Expected: { acknowledged: true, insertedId: 5 }

// The write is visible on main  -- they are the same working set.
db.getSiblingDB("verifydb@main").items.countDocuments({})
// Expected: 3

head.items.deleteOne({ _id: 5 })
// Expected: { acknowledged: true, deletedCount: 1 }

// HEAD~N resolves to the Nth first-parent ancestor of main. It is read-only,
// same as main~N.
const prev = db.getSiblingDB("verifydb@HEAD~1")
prev.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (HEAD~1 is the parent of main's HEAD  -- commit 1, one document.)

prev.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// HEAD^ aliases main^ (first parent of HEAD).  Read-only, like HEAD~1.
var caretDB = db.getSiblingDB("verifydb@HEAD^")
caretDB.items.find({}).toArray()
// Expected: 1 document (first parent = commit 1)

caretDB.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// HEAD^2 is only valid on merge commits (selects the second parent).
// On a non-merge commit, it returns an error because there is no second parent.
```

---

## Scenario 6: reflog syntax  -- returns a clear 'not supported' error

Reflog syntax (`<ref>@{...}`) is not supported. The raw `@`, `{`, `}`, and space
characters are invalid in MongoDB database names, so mongosh rejects them
client-side with `MongoshInvalidInputError: [COMMON-10001] Invalid database name`
before any network call is made. To reach the server-side parser, percent-encode
the special characters: `@` -> `%40`, `{` -> `%7B`, `}` -> `%7D`, space -> `%20`.
DumboDB decodes the DB name server-side (the same mechanism Scenario 2 uses for
`.` in branch names) and returns the documented code-96 rejection.

```js
// main@{yesterday} -> percent-encoded
db.getSiblingDB("verifydb@main%40%7Byesterday%7D").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{yesterday}": reflog syntax is not supported

// main@{5 minutes ago} -> percent-encoded (note %20 for spaces)
db.getSiblingDB("verifydb@main%40%7B5%20minutes%20ago%7D").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{5 minutes ago}": reflog syntax is not supported

// @{1} -> percent-encoded
db.getSiblingDB("verifydb@%40%7B1%7D").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "@{1}": reflog syntax is not supported
```

> **Why not type the raw form?** Typing `verifydb@main@{yesterday}` in mongosh
> fails with a client-side validation error, not the server-side `reflog syntax is
> not supported`. Percent-encoding is the only way a mongosh user can actually reach
> the documented error path. Drivers with permissive DB-name validation (the Go
> mongo-driver among them) accept the raw form and also reach the same server error.

---

## Scenario 7: range syntax  -- returns a clear 'not supported' error

Range syntax (`<ref>..<ref>`) is not supported. The `.` character is forbidden in
MongoDB database names, so mongosh rejects raw range expressions client-side.
Percent-encode `.` as `%2E` (the same encoding used for branch names like `v1.0`
in Scenario 2) to reach the server-side rejection.

```js
// main..feature -> percent-encoded
db.getSiblingDB("verifydb@main%2E%2Efeature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main..feature": range syntax is not supported

// main...feature (three-dot range) -> percent-encoded
db.getSiblingDB("verifydb@main%2E%2E%2Efeature").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main...feature": range syntax is not supported
```

---

## Quick Reference

| Rootish form | Example | Read | Write[1] | Branch creation[2] | Notes |
|---|---|---|---|---|---|
| Branch name (main) | `mydb@main` | yes | yes | yes | Writes go to main's working set |
| Branch name (other) | `mydb@v1%2E0` (encodes `v1.0`) | yes | yes | yes | Writes go to that branch's working set, isolated from main |
| Tag name | `mydb@v1%2E0` (when `v1.0` is a tag) | yes | no | yes | Collection writes blocked; branch creation resolves the tag's commit |
| Commit hash (32 chars) | `mydb@<hash>` | yes | no | yes | Collection writes blocked; branch creation uses the hash directly |
| Ancestor expression | `mydb@main~1` | yes | no | yes | Collection writes blocked; branch creation walks to the Nth ancestor commit |
| HEAD | `mydb@HEAD` | yes | yes | yes | Alias for the default branch (`main`); writes go to main's working set |
| HEAD-relative | `mydb@HEAD~N` | yes | no | yes | Alias for `main~N`; collection writes blocked, branch creation walks ancestry |
| Caret parent | `mydb@main^2` | yes | no | yes | Selects Nth parent (1=first, 2=second on merge commits, 0=self) |
| Chained | `mydb@main^1~2`, `mydb@HEAD^^` | yes | no | yes | Operators compose left to right, matching git |
| Reflog | `mydb@main%40%7Byesterday%7D` (encodes `main@{yesterday}`) | no | no | no | Raw `@{}` invalid in mongosh DB names; percent-encoded form reaches server for code 96 |
| Range | `mydb@main%2E%2Efeature` (encodes `main..feature`) | no | no | no | Raw `.` invalid in mongosh DB names; percent-encoded form reaches server for code 96 |

[1] **Write** = collection mutations (insertOne, updateOne, deleteOne, createCollection, etc.)
[2] **Branch creation** = `db.runCommand({ doltBranch: 1, branch: "newname" })`. Works whenever
the rootish resolves to a commit  -- branch creation only needs a commit address, not write access.

All errors use MongoDB error code **96** (`OperationFailed`).

### Checking the error code in mongosh

```js
try {
  // Reflog syntax is rejected  -- use percent-encoded form so it reaches the server.
  db.getSiblingDB("verifydb@main%40%7Byesterday%7D").items.find({}).toArray()
} catch (e) {
  print("code:", e.code)      // 96
  print("message:", e.message)
}
```
