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

## Setup: Create a database with two commits, a branch, and a tag

Run this once before the verification scenarios below.

```js
var db = db.getSiblingDB("verifydb")
db.dropDatabase()

// Commit 1: one document
db.items.insertOne({ _id: 1, label: "first", version: 1 })
const result1 = db.runCommand({ doltCommit: 1, message: "first commit", author: "alice <alice@acme.com>" })
const hash1 = result1.commitId

// Commit 2: two documents
db.items.insertOne({ _id: 2, label: "second", version: 2 })
const result2 = db.runCommand({ doltCommit: 1, message: "second commit", author: "bob <bob@widgets.io>" })
const hash2 = result2.commitId

// Create a branch "feature" at current main HEAD
db.getSiblingDB("verifydb@main").runCommand({ doltBranch: 1, branch: "feature" })

// Create a tag "release-1" at commit 1
db.runCommand({ dumboTag: 1, name: "release-1", hash: hash1 })

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `verifydb` has:
- **main** (HEAD = commit 2): two documents (`_id: 1` and `_id: 2`)
- **feature**: branch pointing at commit 2 (same as main HEAD)
- **release-1**: tag pointing at commit 1 (one document)
- **hash1**: commit with one document
- **hash2**: commit with two documents (same as main HEAD)

---

## Scenario 1: `verifydb@main` -- reads and writes work

Branch rootish. Full read/write access.

```js
const main = db.getSiblingDB("verifydb@main")

main.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 }, { _id: 2, label: "second", version: 2 } ]

// Write: insert then remove to keep state clean
main.items.insertOne({ _id: 3, label: "third", version: 3 })
main.items.find({}).toArray()
// Expected: three documents

main.items.deleteOne({ _id: 3 })
```

Key checks:
- Reads return both documents
- Writes succeed
- State is restored after cleanup

---

## Scenario 2: `verifydb@feature` -- branch isolation

Non-main branch rootish. Full read/write access; writes go to that branch's working
set and are isolated from main's working set.

```js
const feat = db.getSiblingDB("verifydb@feature")

// Read: same data as main at setup time
feat.items.find({}).toArray()
// Expected: [ { _id: 1, ... }, { _id: 2, ... } ]

// Write on feature: isolated from main
feat.items.insertOne({ _id: 10, label: "feature-only" })

feat.items.countDocuments({})
// Expected: 3

db.getSiblingDB("verifydb@main").items.countDocuments({})
// Expected: 2 (feature write does NOT appear on main)

// Clean up
feat.items.deleteOne({ _id: 10 })
```

Key checks:
- Reads return the same data as main at branch creation time
- Writes succeed on the branch
- Writes are isolated -- main is unchanged

---

## Scenario 3: `verifydb@release-1` -- tag is read-only

Tag rootish. Read-only view of the tagged commit. Writes are blocked.

```js
const tagged = db.getSiblingDB("verifydb@release-1")

// Read: returns data at the tagged commit (commit 1, one document)
tagged.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]

tagged.items.countDocuments({})
// Expected: 1

// Write: must fail -- tags are read-only snapshots
tagged.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: cannot write to a read-only database snapshot

// Branch creation from a tag works -- it resolves to the tagged commit
tagged.runCommand({ doltBranch: 1, branch: "from-tag" })
// Expected: { branch: "from-tag", ok: 1 }

db.getSiblingDB("verifydb@from-tag").items.countDocuments({})
// Expected: 1 (same as the tagged commit)
```

Key checks:
- Reads return data at the tagged commit
- Writes are blocked with code 96
- Branch creation from a tag works

---

## Scenario 4: `verifydb@<hash>` -- commit hash is read-only

Commit hash rootish. Read-only view of the exact snapshot at that commit.

```js
const snap1 = db.getSiblingDB("verifydb@" + hash1)

snap1.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]

snap1.items.countDocuments({})
// Expected: 1

const snap2 = db.getSiblingDB("verifydb@" + hash2)
snap2.items.countDocuments({})
// Expected: 2

// Write on commit hash: must fail
snap1.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96)

// Branch creation from a hash works
snap1.runCommand({ doltBranch: 1, branch: "from-hash1" })
// Expected: { branch: "from-hash1", ok: 1 }

db.getSiblingDB("verifydb@from-hash1").items.countDocuments({})
// Expected: 1
```

Key checks:
- hash1 snapshot has 1 document, hash2 has 2
- Writes are blocked with code 96
- Branch creation works

---

## Scenario 5: `verifydb@main~1` -- ancestor expression is read-only

Ancestor expression rootish. `main~1` is the first parent of main's HEAD.

```js
const parent = db.getSiblingDB("verifydb@main~1")

parent.items.find({}).toArray()
// Expected: [ { _id: 1, label: "first", version: 1 } ]
// (_id:2 was added in commit 2, which is HEAD, not its parent)

parent.items.countDocuments({})
// Expected: 1

// main~0 is main itself
db.getSiblingDB("verifydb@main~0").items.countDocuments({})
// Expected: 2

// Write: must fail
parent.items.insertOne({ _id: 99, label: "should fail" })
// Expected error (code 96)

// Branch creation works
parent.runCommand({ doltBranch: 1, branch: "back-one" })
// Expected: { branch: "back-one", ok: 1 }

db.getSiblingDB("verifydb@back-one").items.countDocuments({})
// Expected: 1
```

Key checks:
- `main~1` returns commit 1 data (one document)
- `main~0` returns current HEAD data (two documents)
- Writes blocked, branch creation works

---

## Scenario 6: HEAD, caret, and chained traversal

HEAD aliases the default branch (`main`). Caret (`^`) selects a specific parent.
Tilde (`~`) walks first-parent ancestors. These operators compose left to right.

### HEAD basics

```js
const head = db.getSiblingDB("verifydb@HEAD")

head.items.find({}).toArray()
// Expected: same as main (2 documents)

// Write via HEAD goes to main's working set
head.items.insertOne({ _id: 5, label: "via-HEAD" })
db.getSiblingDB("verifydb@main").items.countDocuments({})
// Expected: 3

head.items.deleteOne({ _id: 5 })
```

### HEAD~N and HEAD^

```js
// HEAD~1 aliases main~1 (read-only)
db.getSiblingDB("verifydb@HEAD~1").items.countDocuments({})
// Expected: 1

// HEAD^ is the same as HEAD~1 (first parent, read-only)
db.getSiblingDB("verifydb@HEAD^").items.countDocuments({})
// Expected: 1

// Writes blocked on both
db.getSiblingDB("verifydb@HEAD~1").items.insertOne({ _id: 99 })
// Expected error (code 96)
db.getSiblingDB("verifydb@HEAD^").items.insertOne({ _id: 99 })
// Expected error (code 96)
```

### Caret parent selection and chaining (requires merge commit)

To test `^2` and chained expressions, create a merge commit:

```js
// Fresh database for this sub-scenario
var cdb = db.getSiblingDB("chaindb")
cdb.dropDatabase()

cdb.items.insertOne({ _id: 1, v: "root" })
cdb.runCommand({ doltCommit: 1, message: "C1", author: "alice <alice@acme.com>" })
const hashC1 = cdb.runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId

cdb.getSiblingDB("chaindb@main").runCommand({ doltBranch: 1, branch: "feature" })

cdb.getSiblingDB("chaindb@feature").items.insertOne({ _id: 2, v: "feat" })
const rC2 = cdb.getSiblingDB("chaindb@feature").runCommand({ doltCommit: 1, message: "C2-feature", author: "bob <bob@widgets.io>" })
const hashC2 = rC2.commitId

cdb.items.insertOne({ _id: 3, v: "main-adv" })
const rC3 = cdb.runCommand({ doltCommit: 1, message: "C3-main", author: "alice <alice@acme.com>" })
const hashC3 = rC3.commitId

const mergeR = cdb.getSiblingDB("chaindb@main").runCommand({ doltMerge: 1, merge_in: "feature" })
const hashM = mergeR.commitId
```

Now the merge commit M has:
- `M^1` = C3 (main's pre-merge tip)
- `M^2` = C2 (feature's tip)
- `M^0` = M itself
- `M^1~1` = C1 (root)
- `M^^` = C1 (first parent of first parent)

```js
// ^1 = first parent
cdb.getSiblingDB("chaindb@main^1").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashC3

// ^2 = second parent (merge commits only)
cdb.getSiblingDB("chaindb@main^2").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashC2

// ^0 = the commit itself
cdb.getSiblingDB("chaindb@main^0").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashM

// Chained: ^1~1 = parent of first parent = root
cdb.getSiblingDB("chaindb@main^1~1").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashC1

// Chained: ~1^1 = first parent of (first parent of HEAD) = root
cdb.getSiblingDB("chaindb@main~1^1").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashC1

// ^^ = first parent of first parent = root
cdb.getSiblingDB("chaindb@main^^").runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
// Expected: hashC1
```

Key checks:
- `^1` and `^2` select the correct parents of a merge commit
- `^0` returns the commit itself
- Chained expressions compose left to right
- `^^` is equivalent to `^1^1`

---

## Scenario 7: Percent-encoding for special characters in branch names

Characters invalid in MongoDB database names (`.`, `/`, `$`, space) must be
percent-encoded in the rootish. DumboDB decodes them server-side.

Common encodings: `.` -> `%2E`, `/` -> `%2F`, `$` -> `%24`

```js
// Create a branch with a dot in its name
db.getSiblingDB("verifydb@main").runCommand({ doltBranch: 1, branch: "v1.0" })
// Expected: { branch: "v1.0", ok: 1 }

// Access it via percent-encoded name
const v1 = db.getSiblingDB("verifydb@v1%2E0")

v1.items.find({}).toArray()
// Expected: both documents (same as main HEAD at branch creation)

// Write works -- it is a branch, not a tag
v1.items.insertOne({ _id: 20, label: "encoded-branch" })
v1.items.countDocuments({})
// Expected: 3

// main is unchanged
db.getSiblingDB("verifydb@main").items.countDocuments({})
// Expected: 2

// Clean up
v1.items.deleteOne({ _id: 20 })
```

Key checks:
- Percent-encoded branch name resolves correctly
- Full read/write access on the decoded branch
- Isolation from main

---

## Scenario 8: reflog syntax -- not supported

Reflog syntax (`<ref>@{...}`) is not supported. The raw `@`, `{`, `}` characters
are invalid in MongoDB database names, so percent-encode them to reach the server.

```js
// main@{yesterday} -> percent-encoded
db.getSiblingDB("verifydb@main%40%7Byesterday%7D").items.find({}).toArray()
// Expected error (code 96):
//   MongoServerError[OperationFailed]: rootish "main@{yesterday}": ...

// main@{5 minutes ago} -> percent-encoded
db.getSiblingDB("verifydb@main%40%7B5%20minutes%20ago%7D").items.find({}).toArray()
// Expected error (code 96)

// @{1} -> percent-encoded
db.getSiblingDB("verifydb@%40%7B1%7D").items.find({}).toArray()
// Expected error (code 96)
```

---

## Scenario 9: range syntax -- not supported

Range syntax (`<ref>..<ref>`) is not supported. Percent-encode `.` as `%2E`.

```js
// main..feature -> percent-encoded
db.getSiblingDB("verifydb@main%2E%2Efeature").items.find({}).toArray()
// Expected error (code 96)

// main...feature (three-dot) -> percent-encoded
db.getSiblingDB("verifydb@main%2E%2E%2Efeature").items.find({}).toArray()
// Expected error (code 96)
```

---

## Quick Reference

| Rootish form | Example | Read | Write[1] | Branch creation[2] | Notes |
|---|---|---|---|---|---|
| Branch name (main) | `mydb@main` | yes | yes | yes | Writes go to main's working set |
| Branch name (other) | `mydb@feature` | yes | yes | yes | Isolated working set |
| Tag name | `mydb@release-1` | yes | no | yes | Read-only; resolves to tagged commit |
| Commit hash (32 chars) | `mydb@<hash>` | yes | no | yes | Read-only snapshot |
| Ancestor expression | `mydb@main~1` | yes | no | yes | Read-only; walks first-parent chain |
| HEAD | `mydb@HEAD` | yes | yes | yes | Alias for `main` |
| HEAD-relative | `mydb@HEAD~N`, `mydb@HEAD^` | yes | no | yes | Alias for `main~N` / `main^` |
| Caret parent | `mydb@main^2` | yes | no | yes | Selects Nth parent (merge commits) |
| Chained | `mydb@main^1~2`, `mydb@HEAD^^` | yes | no | yes | Operators compose left to right |
| Percent-encoded | `mydb@v1%2E0` (encodes `v1.0`) | yes | yes | yes | Decoded server-side |
| Reflog | `mydb@main%40%7Byesterday%7D` | no | no | no | Not supported (code 96) |
| Range | `mydb@main%2E%2Efeature` | no | no | no | Not supported (code 96) |

[1] **Write** = collection mutations (insertOne, updateOne, deleteOne, etc.)
[2] **Branch creation** = `db.runCommand({ doltBranch: 1, branch: "name" })`. Works whenever
the rootish resolves to a commit -- only needs a commit address, not write access.

All errors use MongoDB error code **96** (`OperationFailed`).
