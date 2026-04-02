# dongoCommit Verification

Manual verification guide for `dongoCommit` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_commit_verify_test.go` (`TestCommitVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCommitVerify -v
> ```

## Parameters

| Parameter   | Type     | Required | Default      | Description                              |
|-------------|----------|----------|--------------|------------------------------------------|
| `message`   | string   | no       | `""`         | Commit message                           |
| `author`    | string   | **yes**  | —            | Name of the commit author                |
| `timestamp` | datetime | no       | current time | Commit timestamp (BSON Date)             |

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with a baseline commit

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("commitdb")
db.dropDatabase()

// Baseline: two documents, committed
db.items.insertOne({ _id: 1, label: "alpha", v: 1 })
db.items.insertOne({ _id: 2, label: "beta",  v: 2 })
const r1 = db.runCommand({ dongoCommit: 1, message: "baseline", author: "alice" })
printjson(r1)
// Expected: { hash: "<hashBase>", branch: "main", message: "baseline", author: "alice", timestamp: ISODate("..."), ok: 1 }
const hashBase = r1.commitId

print("hashBase =", hashBase)
```

After setup, `commitdb` has one commit on `main` with two documents.

---

## Scenario 1: Response shape

`dongoCommit` returns `hash`, `branch`, `message`, `author`, `timestamp`, and `ok`.

The response from setup already demonstrates the shape. Verify each field:

```js
const r = db.runCommand({ dongoCommit: 1, message: "shape check", author: "alice" })
printjson(r)
```

Expected result structure:

```json
{
  "commitId":      "<non-empty hex string>",
  "branch":    "main",
  "message":   "shape check",
  "author":    "alice",
  "timestamp": ISODate("..."),
  "ok":        1
}
```

Key checks:
- `hash` is a non-empty string (Dolt commit hash)
- `branch` reflects the connection's current branch (`"main"` for the plain db name)
- `message` echoes the message you provided
- `author` echoes the author you provided
- `timestamp` is a BSON Date (defaults to current time when not specified)
- `ok` is `1`

---

## Scenario 2: Commit on a named branch — data is committed to that branch

Create a branch, connect to it, insert a document, commit. Verify that the committed
data is visible on the branch but not on main (isolation check).

```js
// Create branch "feature" from main HEAD
db.getSiblingDB("commitdb__main").runCommand({ dongoBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

var feature = db.getSiblingDB("commitdb__feature")
feature.items.insertOne({ _id: 3, label: "gamma", v: 3 })

const r2 = feature.runCommand({ dongoCommit: 1, message: "feature commit", author: "alice" })
printjson(r2)
// Expected: { hash: "<hash>", branch: "<branch>", message: "feature commit", author: "alice", timestamp: ISODate("..."), ok: 1 }

// Verify isolation: feature has the new doc, main does not
feature.items.countDocuments({})
// Expected: 3 (two from setup + _id:3)

db.getSiblingDB("commitdb__main").items.countDocuments({})
// Expected: 2 (feature commit must not affect main)
```

Key checks:
- `hash` is non-empty, `message` echoes `"feature commit"`, `ok` is `1`
- `feature` branch has 3 documents after the commit
- `main` branch still has 2 documents — the feature commit did not affect main

---

## Scenario 3: Successive commits have distinct hashes

Each `dongoCommit` call creates a new, unique commit hash. Two sequential commits
to the same database must return different hash values.

```js
// Make a change and commit
db.items.insertOne({ _id: 10, label: "ten", v: 10 })
const r3a = db.runCommand({ dongoCommit: 1, message: "commit A", author: "alice" })
print("hashA =", r3a.commitId)

db.items.insertOne({ _id: 11, label: "eleven", v: 11 })
const r3b = db.runCommand({ dongoCommit: 1, message: "commit B", author: "alice" })
print("hashB =", r3b.commitId)

print("hashes differ:", r3a.commitId !== r3b.commitId)
// Expected: true
```

Key check: `r3a.commitId !== r3b.commitId`.

---

## Scenario 4: Commit on empty working set

When no changes are pending since the last commit, `dongoCommit` still succeeds.

```js
// No changes since last commit
const r4 = db.runCommand({ dongoCommit: 1, message: "empty", author: "alice" })
printjson(r4)
```

Expected: `{ hash: "<hash>", branch: "main", message: "empty", author: "alice", timestamp: ISODate("..."), ok: 1 }`

Key check: `ok` is `1`; `hash` is non-empty.

---

## Scenario 5: Committed hash is a valid diff reference

The hash returned by `dongoCommit` can immediately be used as a `from` or `to`
argument in `dongoDiff`.

```js
// Record state before a change
const hashBefore = db.runCommand({ dongoCommit: 1, message: "pre-change", author: "alice" }).commitId

// Make a change and commit
db.items.insertOne({ _id: 99, label: "new", v: 99 })
const hashAfter = db.runCommand({ dongoCommit: 1, message: "post-change", author: "alice" }).commitId

// Diff between the two commits — must show _id:99 as added
db.runCommand({ dongoDiff: 1, from: hashBefore, to: hashAfter })
```

Expected: `added` contains exactly `_id:99`; `collections` is non-empty.

---

## Scenario 6: Author is echoed and visible in dongoLog

The `author` provided to `dongoCommit` is echoed in the response and stored in the
commit — it is visible via `dongoLog`.

```js
const r6 = db.runCommand({ dongoCommit: 1, message: "authored commit", author: "bob" })
printjson(r6)
// Expected: { hash: "...", branch: "main", message: "authored commit", author: "bob", timestamp: ISODate("..."), ok: 1 }

// Verify author appears in dongoLog
const log = db.runCommand({ dongoLog: 1, limit: 1 })
print("log author:", log.commits[0].author)
// Expected: "bob"
```

Key checks:
- `r6.author` equals `"bob"`
- `log.commits[0].author` equals `"bob"`

---

## Scenario 7: Custom timestamp is stored and echoed

Pass a BSON Date as `timestamp` to pin the commit to a specific point in time.
The value is echoed in the response and stored in the commit (visible via `dongoLog`).

```js
const fixedTime = new Date("2020-06-15T12:00:00Z")
const r7 = db.runCommand({
  dongoCommit: 1,
  message:     "fixed-time commit",
  author:      "carol",
  timestamp:   fixedTime,
})
printjson(r7)
// Expected: { ..., author: "carol", timestamp: ISODate("2020-06-15T12:00:00Z"), ok: 1 }

// Verify timestamp appears in dongoLog
const log7 = db.runCommand({ dongoLog: 1, limit: 1 })
print("log timestamp:", log7.commits[0].timestamp)
// Expected: ISODate("2020-06-15T12:00:00Z")
```

Key checks:
- `r7.timestamp` equals `fixedTime`
- `log7.commits[0].timestamp` equals `fixedTime`

---

## Quick Reference

| Scenario | Command | Key outcome |
|---|---|---|
| Commit on main | `{ dongoCommit: 1, message: "msg", author: "alice" }` | `{ hash, branch:"main", message:"msg", author:"alice", timestamp:ISODate(...), ok:1 }` |
| Commit on branch | `featureDB.runCommand({ dongoCommit: 1, ..., author: "alice" })` | Data committed to branch; isolation verified via count |
| Two sequential commits | Call twice with same author | Hashes are different |
| Empty working set | Commit with no pending changes | Succeeds with `ok:1` |
| Use hash in diff | `{ dongoDiff: 1, from: hash1, to: hash2 }` | Shows changes between commits |
| Custom author | Pass `author: "bob"` | Response and dongoLog echo `"bob"` |
| Custom timestamp | Pass `timestamp: new Date("2020-06-15")` | Response and dongoLog echo fixed time |

- `hash` is a Dolt commit hash (non-empty string).
- `branch` reflects the connection's branch, not the database base name.
- `message` is echoed verbatim in the response.
- `author` is required; it is stored and visible in `dongoLog`.
- `timestamp` is optional; omit it to use the current server time.
