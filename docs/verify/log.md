# dongoLog Verification

Manual verification guide for `dongoLog` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_log_verify_test.go` (`TestLogVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestLogVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Scenario 1: Log with no user commits — only the "Initialize database" root

Every Dongo database is created with an automatic `"Initialize database"` root
commit. Before any user commits, `dongoLog` returns exactly that one commit.

```js
var db = db.getSiblingDB("logdb")
db.dropDatabase()

db.items.insertOne({ _id: 0 })

db.runCommand({ dongoLog: 1 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "hash": "<initHash>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
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
const r1 = db.runCommand({ dongoCommit: 1, message: "first", author: "alice" })
printjson(r1)
// Expected: { hash: "<hash1>", branch: "main", message: "first", ok: 1 }
const hash1 = r1.hash

db.items.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ dongoCommit: 1, message: "second", author: "alice" })
printjson(r2)
const hash2 = r2.hash

db.items.insertOne({ _id: 3, label: "gamma" })
const r3 = db.runCommand({ dongoCommit: 1, message: "third", author: "alice" })
printjson(r3)
const hash3 = r3.hash

print("hash1 =", hash1, "hash2 =", hash2, "hash3 =", hash3)
```

After setup, the branch has four commits: `"Initialize database"` ← `first` ← `second` ← `third` (HEAD).

---

## Scenario 2: Log after multiple commits — parent chain, newest-first

`dongoLog` returns all commits newest-first. Including the `"Initialize database"`
root, four entries appear in total.

```js
db.runCommand({ dongoLog: 1 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "hash": "<hash3>", "parent1": "<hash2>", "message": "third",               "timestamp": "<...>", "author": "<...>" },
    { "hash": "<hash2>", "parent1": "<hash1>", "message": "second",              "timestamp": "<...>", "author": "<...>" },
    { "hash": "<hash1>", "parent1": "<initH>", "message": "first",               "timestamp": "<...>", "author": "<...>" },
    { "hash": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Four entries returned, newest-first
- `commits[0].hash` equals `hash3`, `commits[0].parent1` equals `hash2`
- `commits[1].hash` equals `hash2`, `commits[1].parent1` equals `hash1`
- `commits[2].hash` equals `hash1`, `commits[2].parent1` points to the Initialize root
- `commits[3].message` is `"Initialize database"`, no `parent1`

---

## Scenario 3: Log with limit — truncates at the specified count

`dongoLog` with `limit: 2` returns at most 2 entries starting from HEAD.

```js
db.runCommand({ dongoLog: 1, limit: 2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "hash": "<hash3>", "message": "third",  "timestamp": "<...>", "author": "<...>" },
    { "hash": "<hash2>", "message": "second", "timestamp": "<...>", "author": "<...>" }
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

`dongoLog` with `from: hash2` starts at `hash2` and walks backwards, skipping
the HEAD commit (`hash3`). The walk continues through `hash1` down to the
`"Initialize database"` root — three entries in total.

```js
db.runCommand({ dongoLog: 1, from: hash2 })
```

Expected:

```json
{
  "branch": "main",
  "commits": [
    { "hash": "<hash2>", "message": "second",              "timestamp": "<...>", "author": "<...>" },
    { "hash": "<hash1>", "message": "first",               "timestamp": "<...>", "author": "<...>" },
    { "hash": "<initH>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- `hash3` ("third") does **not** appear — traversal starts from `hash2`
- Three entries: `hash2`, `hash1`, then the Initialize root
- The Initialize root entry has no `parent1`

---

## Quick Reference

| Command | Behaviour |
|---|---|
| `{ dongoLog: 1 }` | All commits from HEAD, up to default limit (20) |
| `{ dongoLog: 1, limit: N }` | At most N commits from HEAD |
| `{ dongoLog: 1, from: "<hash>" }` | All commits from `<hash>` backwards |
| `{ dongoLog: 1, from: "<hash>", limit: N }` | At most N commits from `<hash>` |

- Commits are returned newest-first.
- Each entry contains `hash`, `message`, `timestamp`, and `author`.
- `parent1` is present on all non-root commits.
- `parent2` is present only on merge commits.
- The root commit has neither `parent1` nor `parent2`.
