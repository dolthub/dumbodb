# docudoltLog Verification

Manual verification guide for `docudoltLog` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_log_verify_test.go` (`TestLogVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestLogVerify -v
> ```

## Prerequisites

A running Docudolt instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Docudolt address if different.

---

## Scenario 1: Log with no user commits — only the "Initialize database" root

Every Docudolt database is created with an automatic `"Initialize database"` root
commit. Before any user commits, `docudoltLog` returns exactly that one commit.

```js
var db = db.getSiblingDB("logdb")
db.dropDatabase()

db.items.insertOne({ _id: 0 })

db.runCommand({ docudoltLog: 1 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<initHash>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 1 commit: `"Initialize database"`
- No `parent1` field — it is the root commit

---

## Setup: Create a commit history

```js
db.items.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ docudoltCommit: 1, message: "first", author: "alice <alice@docudolt>" })
printjson(r1)
// Expected: { hash: "<hash1>", branch: "main", message: "first", ok: 1 }
const hash1 = r1.commitId

db.items.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ docudoltCommit: 1, message: "second", author: "alice <alice@docudolt>" })
printjson(r2)
const hash2 = r2.commitId

db.items.insertOne({ _id: 3, label: "gamma" })
const r3 = db.runCommand({ docudoltCommit: 1, message: "third", author: "alice <alice@docudolt>" })
printjson(r3)
const hash3 = r3.commitId

print("hash1 =", hash1, "hash2 =", hash2, "hash3 =", hash3)
```

After setup, the branch has four commits: `"Initialize database"` ← `first` ← `second` ← `third` (HEAD).

---

## Scenario 2: Log after multiple commits — parent chain, newest-first

`docudoltLog` returns all commits newest-first. Including the `"Initialize database"`
root, four entries appear in total.

```js
db.runCommand({ docudoltLog: 1 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<hash3>", "parent1": "<hash2>", "message": "third",               "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hash2>", "parent1": "<hash1>", "message": "second",              "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hash1>", "parent1": "<initH>", "message": "first",               "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Four entries returned, newest-first
- `commits[0].commitId` equals `hash3`, `commits[0].parent1` equals `hash2`
- `commits[1].commitId` equals `hash2`, `commits[1].parent1` equals `hash1`
- `commits[2].commitId` equals `hash1`, `commits[2].parent1` points to the Initialize root
- `commits[3].message` is `"Initialize database"`, no `parent1`

---

## Scenario 3: Log with limit — truncates at the specified count

`docudoltLog` with `limit: 2` returns at most 2 entries starting from HEAD.

```js
db.runCommand({ docudoltLog: 1, limit: 2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<hash3>", "message": "third",  "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hash2>", "message": "second", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 2 entries
- First entry is `hash3` (HEAD), second is `hash2`
- `hash1` ("first") does **not** appear

---

## Scenario 4: Log from a specific hash — start traversal at that commit

`docudoltLog` with `from: hash2` starts at `hash2` and walks backwards, skipping
the HEAD commit (`hash3`). The walk continues through `hash1` down to the
`"Initialize database"` root — three entries in total.

```js
db.runCommand({ docudoltLog: 1, from: hash2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<hash2>", "message": "second",              "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hash1>", "message": "first",               "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<initH>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- `hash3` ("third") does **not** appear — traversal starts from `hash2`
- Three entries: `hash2`, `hash1`, then the Initialize root
- The Initialize root entry has no `parent1`

---

## Scenario 5: Refs annotation — branch head decoration (git --decorate)

When a commit is the HEAD of one or more branches its entry includes a `refs`
array.  The connection branch gets two separate entries: `"HEAD"` and the bare
branch name.  Other branches get only their bare name.  Non-head commits have
no `refs` field.

```js
// Create a second branch pointing at the current main HEAD.
db.getSiblingDB("logdb__d_main").runCommand({ docudoltBranch: 1, branch: "feature" })

// Query from main — hash3 is tip of both "main" and "feature".
db.runCommand({ docudoltLog: 1, limit: 2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    {
      "commitId": "<hash3>",
      "refs": ["HEAD", "main", "feature"],
      "parent1": "<hash2>",
      "message": "third",
      "timestamp": "<...>",
      "author": "<...>"
    },
    {
      "commitId": "<hash2>",
      "parent1": "<hash1>",
      "message": "second",
      "timestamp": "<...>",
      "author": "<...>"
    }
  ],
  "ok": 1
}
```

Key checks:
- `commits[0].refs` contains `"HEAD"` and `"main"` (connection branch gets HEAD + bare name)
- `commits[0].refs` also contains `"feature"` (bare name for the other branch)
- `commits[1]` has no `refs` field (non-head commit)
- When the same command runs against `logdb__d_feature`, `commits[0].refs` becomes
  `["HEAD", "feature", "main"]`

---

---

## Setup: Create a non-linear history with a merge commit

The next three scenarios require a database that has a true three-way merge.

```js
var mdb = db.getSiblingDB("logmerge")
mdb.dropDatabase()

mdb.items.insertOne({ _id: 1, v: 1 })
const rA = mdb.runCommand({ docudoltCommit: 1, message: "add-one", author: "alice <alice@docudolt>" })
const hashA = rA.commitId

// Create "feat" branch from main HEAD (hashA).
mdb.getSiblingDB("logmerge__d_main").runCommand({ docudoltBranch: 1, branch: "feat" })

// Advance main: _id:2 → hashB.
mdb.items.insertOne({ _id: 2, v: 2 })
const rB = mdb.runCommand({ docudoltCommit: 1, message: "add-two", author: "alice <alice@docudolt>" })
const hashB = rB.commitId

// Advance feat independently: _id:3 → hashC.
const featdb = db.getSiblingDB("logmerge__d_feat")
featdb.items.insertOne({ _id: 3, v: 3 })
const rC = featdb.runCommand({ docudoltCommit: 1, message: "add-three-feat", author: "alice <alice@docudolt>" })
const hashC = rC.commitId

// Merge feat into main → hashM.
const rM = mdb.getSiblingDB("logmerge__d_main").runCommand({ docudoltMerge: 1, merge_in: "feat" })
const hashM = rM.commitId

print("hashA =", hashA, "hashB =", hashB, "hashC =", hashC, "hashM =", hashM)
```

After setup the commit graph looks like this:

```
init ← hashA ← hashB (main)
             ↖
              hashC (feat)  →  hashM (HEAD on main, parent1=hashB, parent2=hashC)
```

`docudoltLog` follows `parent1` linearly, so the walk from main HEAD is:
`hashM → hashB → hashA → init`.

---

## Scenario 6: Merge commit appears in docudoltLog with parent1 and parent2

```js
mdb.getSiblingDB("logmerge__d_main").runCommand({ docudoltLog: 1, limit: 1 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    {
      "commitId": "<hashM>",
      "parent1": "<hashB>",
      "parent2": "<hashC>",
      "refs": ["HEAD", "main"],
      "message": "Merge branch 'feat' into 'main'",
      "timestamp": "<...>",
      "author": "<...>"
    }
  ],
  "ok": 1
}
```

Key checks:
- `commits[0].commitId` equals `hashM`
- `commits[0].parent1` equals `hashB` (main tip before the merge)
- `commits[0].parent2` equals `hashC` (feature tip)
- `commits[0].refs` contains `"HEAD"` and `"main"`

---

## Scenario 7: docudoltLog from feature tip shows only feature branch history

Starting traversal at `hashC` follows `parent1` only: `hashC → hashA → init`.
`hashB` (reachable only via main) and `hashM` (the merge commit) must **not** appear.

```js
mdb.getSiblingDB("logmerge__d_main").runCommand({ docudoltLog: 1, from: hashC })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<hashC>", "parent1": "<hashA>", "message": "add-three-feat", "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hashA>", "parent1": "<initH>", "message": "add-one",        "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Three commits: `hashC`, `hashA`, then `"Initialize database"`
- `hashB` (main-only commit) does **not** appear
- `hashM` (merge commit) does **not** appear
- The root commit has no `parent1`

---

## Scenario 8: limit works correctly on non-linear history

`limit=2` from main HEAD follows `parent1`: `hashM → hashB`. Commits further back
(`hashA`, init) must **not** appear.

```js
mdb.getSiblingDB("logmerge__d_main").runCommand({ docudoltLog: 1, limit: 2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "commitId": "<hashM>", "parent1": "<hashB>", "parent2": "<hashC>", "message": "Merge branch 'feat' into 'main'", "timestamp": "<...>", "author": "<...>" },
    { "commitId": "<hashB>", "parent1": "<hashA>", "message": "add-two", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 2 entries
- `commits[0]` is the merge commit `hashM` with both `parent1` and `parent2`
- `commits[1]` is `hashB` (parent1 of the merge commit)
- `hashA` and `hashC` do **not** appear

---

## Quick Reference

| Command | Behaviour |
|---|---|
| `{ docudoltLog: 1 }` | All commits from HEAD, up to default limit (20) |
| `{ docudoltLog: 1, limit: N }` | At most N commits from HEAD |
| `{ docudoltLog: 1, from: "<hash>" }` | All commits from `<hash>` backwards |
| `{ docudoltLog: 1, from: "<hash>", limit: N }` | At most N commits from `<hash>` |

- Commits are returned newest-first.
- Each entry contains `hash`, `message`, `timestamp`, and `author`.
- `parent1` is present on all non-root commits.
- `parent2` is present only on merge commits.
- The root commit has neither `parent1` nor `parent2`.
