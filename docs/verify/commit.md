# doltCommit Verification

Manual verification guide for `doltCommit` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/commit_test.go` (`TestCommitVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCommitVerify -v
> ```

## Parameters

| Parameter    | Type     | Required | Default      | Description                                                                  |
|--------------|----------|----------|--------------|------------------------------------------------------------------------------|
| `message`    | string   | no       | `""`         | Commit message                                                               |
| `author`     | string   | no       | `"dumbodb <dumbodb@dumbodb>"` | `Name <email>` of the commit author. **Only accepted with `--auth` off.**     |
| `timestamp`  | datetime | no       | current time | Commit timestamp (BSON Date)                                                 |
| `allowEmpty` | bool     | no       | `false`      | When true, create a commit even if the working set has no changes vs HEAD    |

> **Authentication note.** Under `--auth`, `doltCommit` **rejects** a client-supplied
> `author`/`committer` with `IDLUnknownField` (40415): the server stamps the
> authenticated user's identity (see `docs/design/commit-identity.md`). The `author`
> column above and the scenarios below assume `--auth` is off.

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with a baseline commit

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("commitdb")

// Baseline: two documents, committed
db.orders.insertOne({ _id: 1, label: "alpha", v: 1 })
db.orders.insertOne({ _id: 2, label: "beta",  v: 2 })
const r1 = db.runCommand({ doltCommit: 1, message: "baseline", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { hash: "<hashBase>", branch: "main", message: "baseline", author: "alice <alice@acme.com>", timestamp: ISODate("..."), committer: "alice <alice@acme.com>", committerTimestamp: ISODate("..."), ok: 1 }
const hashBase = r1.commitId

print("hashBase =", hashBase)
```

After setup, `commitdb` has one commit on `main` with two documents.

---

## Scenario 1: Response shape

`doltCommit` returns `hash`, `branch`, `message`, `author`, `timestamp`, `committer`, `committerTimestamp`, and `ok`.

The response from setup already demonstrates the shape. Verify each field after a real change:

```js
db.orders.insertOne({ _id: 100, label: "shape", v: 100 })
const r = db.runCommand({ doltCommit: 1, message: "shape check", author: "alice <alice@acme.com>" })
printjson(r)
```

Expected result structure:

```json
{
  "commitId":           "<non-empty hex string>",
  "branch":             "main",
  "message":            "shape check",
  "author":             "alice <alice@acme.com>",
  "timestamp":          ISODate("..."),
  "committer":          "alice <alice@acme.com>",
  "committerTimestamp": ISODate("..."),
  "ok":                 1
}
```

Key checks:
- `hash` is a non-empty string (Dolt commit hash)
- `branch` reflects the connection's current branch (`"main"` for the plain db name)
- `message` echoes the message you provided
- `author` echoes the author you provided
- `timestamp` is a BSON Date (defaults to current time when not specified)
- `committer` equals `author` for regular commits
- `committerTimestamp` is a BSON Date (the time the commit was applied)
- `ok` is `1`

---

## Scenario 2: Commit on a named branch  -- data is committed to that branch

Create a branch, connect to it, insert a document, commit. Verify that the committed
data is visible on the branch but not on main (isolation check).

```js
// Create branch "feature" from main HEAD
db.getSiblingDB("commitdb@main").runCommand({ doltBranch: 1, action: "add", branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

var feature = db.getSiblingDB("commitdb@feature")
feature.orders.insertOne({ _id: 3, label: "gamma", v: 3 })

const r2 = feature.runCommand({ doltCommit: 1, message: "feature commit", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { hash: "<hash>", branch: "<branch>", message: "feature commit", author: "bob <bob@widgets.io>", timestamp: ISODate("..."), ok: 1 }

// Verify isolation: feature has the new doc, main does not
feature.orders.countDocuments({})
// Expected: 4 (three from setup + Scenario 1 + _id:3)

db.getSiblingDB("commitdb@main").orders.countDocuments({})
// Expected: 3 (feature commit must not affect main)
```

Key checks:
- `hash` is non-empty, `message` echoes `"feature commit"`, `ok` is `1`
- `feature` branch has 4 documents after the commit
- `main` branch still has 3 documents  -- the feature commit did not affect main

---

## Scenario 3: Successive commits have distinct hashes

Each `doltCommit` call creates a new, unique commit hash. Two sequential commits
to the same database must return different hash values.

```js
// Make a change and commit
db.orders.insertOne({ _id: 10, label: "ten", v: 10 })
const r3a = db.runCommand({ doltCommit: 1, message: "commit A", author: "alice <alice@acme.com>" })
print("hashA =", r3a.commitId)

db.orders.insertOne({ _id: 11, label: "eleven", v: 11 })
const r3b = db.runCommand({ doltCommit: 1, message: "commit B", author: "bob <bob@widgets.io>" })
print("hashB =", r3b.commitId)

print("hashes differ:", r3a.commitId !== r3b.commitId)
// Expected: true
```

Key check: `r3a.commitId !== r3b.commitId`.

---

## Scenario 4: Commit on empty working set

By default, `doltCommit` rejects an empty commit (no changes since the last commit). Pass
`allowEmpty: true` to opt in to creating an empty commit.

### 4a  -- Empty without flag returns an error

```js
// No changes since last commit
const r4a = db.runCommand({ doltCommit: 1, message: "empty", author: "alice <alice@acme.com>" })
printjson(r4a)
```

Expected: `{ ok: 0, errmsg: "doltCommit: nothing to commit, working tree clean", code: 96, ... }`

Key checks:
- `ok` is `0`
- `errmsg` mentions "nothing to commit"
- no `commitId` is returned
- HEAD is unchanged (verify with `db.runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId` before and after)

### 4b  -- Empty with `allowEmpty: true` succeeds and advances HEAD

```js
const headBefore = db.runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId

const r4b = db.runCommand({
  doltCommit: 1,
  message:    "empty allowed",
  author:     "alice <alice@acme.com>",
  allowEmpty: true,
})
printjson(r4b)

const headAfter = db.runCommand({ doltLog: 1, limit: 1 }).commits[0].commitId
print("hashes differ:", headBefore !== headAfter)
// Expected: true
```

Expected: `{ commitId: "<new hash>", branch: "main", message: "empty allowed", author: "...", timestamp: ISODate("..."), ok: 1 }`

Key checks:
- `ok` is `1`
- `commitId` is non-empty
- `commitId` differs from `headBefore` (a new commit was created even though no data changed)

### 4c  -- Repeated bare empty commits keep failing the same way

```js
const r4c = db.runCommand({ doltCommit: 1, message: "still empty", author: "alice <alice@acme.com>" })
printjson(r4c)
```

Expected: `{ ok: 0, errmsg: "doltCommit: nothing to commit, working tree clean", code: 96, ... }`

Key check: the empty commit from 4b cleared no working changes (none existed), so the gate fires again.

---

## Scenario 5: Committed hash is a valid diff reference

The hash returned by `doltCommit` can immediately be used as a `from` or `to`
argument in `doltDiff`.

```js
// Insert a doc and commit so hashBefore captures a real change.
db.orders.insertOne({ _id: 50, label: "pre", v: 50 })
const hashBefore = db.runCommand({ doltCommit: 1, message: "pre-change", author: "alice <alice@acme.com>" }).commitId

// Make another change and commit
db.orders.insertOne({ _id: 99, label: "new", v: 99 })
const hashAfter = db.runCommand({ doltCommit: 1, message: "post-change", author: "bob <bob@widgets.io>" }).commitId

// Diff between the two commits  -- must show _id:99 as added
db.runCommand({ doltDiff: 1, from: hashBefore, to: hashAfter })
```

Expected: `changes` is non-empty; the `orders` entry's `documents.added` contains
exactly `_id:99`.

---

## Scenario 6: Author is echoed and visible in doltLog

The `author` you pass is echoed verbatim in the `doltCommit` response and stored
in the commit. The stored author is normalized to `Name <email>` form: when you
pass a bare name with no email, the server synthesizes `<name@dumbodb>`, so
`doltLog` shows the normalized value.

```js
db.orders.insertOne({ _id: 60, label: "author", v: 60 })
const r6 = db.runCommand({ doltCommit: 1, message: "authored commit", author: "bob" })
printjson(r6)
// Expected: { hash: "...", branch: "main", message: "authored commit", author: "bob", timestamp: ISODate("..."), ok: 1 }

// Verify author appears in doltLog (normalized to Name <email>)
const log = db.runCommand({ doltLog: 1, limit: 1 })
print("log author:", log.commits[0].author)
// Expected: "bob <bob@dumbodb>"
```

Key checks:
- `r6.author` equals `"bob"` (the response echoes what you passed)
- `log.commits[0].author` equals `"bob <bob@dumbodb>"` (stored author, email synthesized)

---

## Scenario 7: Custom timestamp is stored and echoed

Pass a BSON Date as `timestamp` to pin the commit to a specific point in time.
The value is echoed in the response and stored in the commit (visible via `doltLog`).

```js
const fixedTime = new Date("2020-06-15T12:00:00Z")
db.orders.insertOne({ _id: 70, label: "timestamp", v: 70 })
const r7 = db.runCommand({
  doltCommit: 1,
  message:     "fixed-time commit",
  author:      "carol",
  timestamp:   fixedTime,
})
printjson(r7)
// Expected: { ..., author: "carol", timestamp: ISODate("2020-06-15T12:00:00Z"), ok: 1 }

// Verify timestamp appears in doltLog
const log7 = db.runCommand({ doltLog: 1, limit: 1 })
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
| Commit on main | `{ doltCommit: 1, message: "msg", author: "alice <alice@acme.com>" }` | `{ hash, branch:"main", message:"msg", author:"alice <alice@acme.com>", timestamp:ISODate(...), ok:1 }` |
| Commit on branch | `featureDB.runCommand({ doltCommit: 1, ..., author: "bob <bob@widgets.io>" })` | Data committed to branch; isolation verified via count |
| Two sequential commits | Call twice with same author | Hashes are different |
| Empty working set, no flag | Commit with no pending changes | Fails with `ok:0` and "nothing to commit" |
| Empty working set, `allowEmpty:true` | Commit with no pending changes plus the flag | Succeeds with `ok:1` and a new hash |
| Use hash in diff | `{ doltDiff: 1, from: hash1, to: hash2 }` | Shows changes between commits |
| Custom author | Pass `author: "bob"` | Response echoes `"bob"`; doltLog shows stored `"bob <bob@dumbodb>"` |
| Custom timestamp | Pass `timestamp: new Date("2020-06-15")` | Response and doltLog echo fixed time |

- `hash` is a Dolt commit hash (non-empty string).
- `branch` reflects the connection's branch, not the database base name.
- `message` is echoed verbatim in the response.
- `author` is required; it is stored and visible in `doltLog`.
- `timestamp` is optional; omit it to use the current server time.
- `committer` equals `author` for regular commits (differs for cherry-pick and rebase).
- `committerTimestamp` records when the commit was applied.
