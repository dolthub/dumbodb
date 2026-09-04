# doltLog Verification

Manual verification guide for `doltLog` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/log_test.go` (`TestLogVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestLogVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Scenario 1: Log with no user commits  -- only the "Initialize database" root

Every DumboDB database is created with an automatic `"Initialize database"` root
commit. Before any user commits, `doltLog` returns exactly that one commit.

```js
var db = db.getSiblingDB("logdb")

db.events.insertOne({ _id: 0 })

db.runCommand({ doltLog: 1 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<initHash>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 1 commit: `"Initialize database"`
- No `parent1` field  -- it is the root commit
- Response has no top-level `branch` field

---

## Setup: Create a commit history

```js
db.events.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ doltCommit: 1, message: "first", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { hash: "<hash1>", branch: "main", message: "first", ok: 1 }
const hash1 = r1.commitId

db.events.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ doltCommit: 1, message: "second", author: "bob <bob@widgets.io>" })
printjson(r2)
const hash2 = r2.commitId

db.events.insertOne({ _id: 3, label: "gamma" })
const r3 = db.runCommand({ doltCommit: 1, message: "third", author: "carol <carol@startup.dev>" })
printjson(r3)
const hash3 = r3.commitId

print("hash1 =", hash1, "hash2 =", hash2, "hash3 =", hash3)
```

After setup, the branch has four commits: `"Initialize database"` <- `first` <- `second` <- `third` (HEAD).

---

## Scenario 2: Log after multiple commits  -- parent chain, newest-first

`doltLog` returns all commits newest-first. Including the `"Initialize database"`
root, four entries appear in total.

```js
db.runCommand({ doltLog: 1 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hash3>", "parent1": "<hash2>", "message": "third",               "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hash2>", "parent1": "<hash1>", "message": "second",              "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hash1>", "parent1": "<initH>", "message": "first",               "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
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

## Scenario 3: Log with limit  -- truncates at the specified count

`doltLog` with `limit: 2` returns at most 2 entries starting from HEAD.

```js
db.runCommand({ doltLog: 1, limit: 2 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hash3>", "message": "third",  "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hash2>", "message": "second", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 2 entries
- First entry is `hash3` (HEAD), second is `hash2`
- `hash1` ("first") does **not** appear

---

## Scenario 3b: Log with limit=0  -- returns an empty list

`limit: 0` explicitly requests zero commits. The response must return an empty
`commits` array regardless of how much history exists, and the same holds when
combined with `from`.

```js
db.runCommand({ doltLog: 1, limit: 0 })
db.runCommand({ doltLog: 1, from: hash2, limit: 0 })
```

Expected for both:

```json
{ "commits": [], "ok": 1 }
```

Key checks:
- `commits` is an empty array
- No top-level `branch` field in the response

---

## Scenario 4: Log from a specific hash  -- start traversal at that commit

`doltLog` with `from: hash2` starts at `hash2` and walks backwards, skipping
the HEAD commit (`hash3`). The walk continues through `hash1` down to the
`"Initialize database"` root  -- three entries in total.

```js
db.runCommand({ doltLog: 1, from: hash2 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hash2>", "message": "second",              "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hash1>", "message": "first",               "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<initH>", "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- `hash3` ("third") does **not** appear  -- traversal starts from `hash2`
- Three entries: `hash2`, `hash1`, then the Initialize root
- The Initialize root entry has no `parent1`

---

## Scenario 5: Refs annotation  -- branch head decoration (git --decorate)

When a commit is the HEAD of one or more branches its entry includes a `refs`
array.  The connection branch gets two separate entries: `"HEAD"` and the bare
branch name.  Other branches get only their bare name.  Non-head commits have
no `refs` field.

```js
// Create a second branch pointing at the current main HEAD.
db.getSiblingDB("logdb@main").runCommand({ doltBranch: 1, action: "add", branch: "feature" })

// Query from main  -- hash3 is tip of both "main" and "feature".
db.runCommand({ doltLog: 1, limit: 2 })
```

Expected:

```json
{
  "commits": [
    {
      "commitId": "<hash3>",
      "refs": ["HEAD", "main", "feature"],
      "parent1": "<hash2>",
      "message": "third",
      "timestamp": "<...>",
      "author": "<...>",
      "committer": "<...>",
      "committerTimestamp": "<...>"
    },
    {
      "commitId": "<hash2>",
      "parent1": "<hash1>",
      "message": "second",
      "timestamp": "<...>",
      "author": "<...>",
      "committer": "<...>",
      "committerTimestamp": "<...>"
    }
  ],
  "ok": 1
}
```

Key checks:
- `commits[0].refs` contains `"HEAD"` and `"main"` (connection branch gets HEAD + bare name)
- `commits[0].refs` also contains `"feature"` (bare name for the other branch)
- `commits[1]` has no `refs` field (non-head commit)
- When the same command runs against `logdb@feature`, `commits[0].refs` becomes
  `["HEAD", "feature", "main"]`

---

---

## Setup: Create a non-linear history with a merge commit

The next three scenarios require a database that has a true three-way merge.

```js
var mdb = db.getSiblingDB("logmerge")

mdb.events.insertOne({ _id: 1, v: 1 })
const rA = mdb.runCommand({ doltCommit: 1, message: "add-one", author: "alice <alice@acme.com>" })
const hashA = rA.commitId

// Create "feat" branch from main HEAD (hashA).
mdb.getSiblingDB("logmerge@main").runCommand({ doltBranch: 1, action: "add", branch: "feat" })

// Advance main: _id:2 -> hashB.
mdb.events.insertOne({ _id: 2, v: 2 })
const rB = mdb.runCommand({ doltCommit: 1, message: "add-two", author: "bob <bob@widgets.io>" })
const hashB = rB.commitId

// Advance feat independently: _id:3 -> hashC.
const featdb = db.getSiblingDB("logmerge@feat")
featdb.events.insertOne({ _id: 3, v: 3 })
const rC = featdb.runCommand({ doltCommit: 1, message: "add-three-feat", author: "carol <carol@startup.dev>" })
const hashC = rC.commitId

// Merge feat into main -> hashM.
const rM = mdb.getSiblingDB("logmerge@main").runCommand({ doltMerge: 1, mergeIn: "feat" })
const hashM = rM.commitId

print("hashA =", hashA, "hashB =", hashB, "hashC =", hashC, "hashM =", hashM)
```

After setup the commit graph looks like this:

```
init <- hashA <- hashB (main)
             \
              hashC (feat)  ->  hashM (HEAD on main, parent1=hashB, parent2=hashC)
```

`doltLog` walks the commit graph in reverse topological order, visiting **both**
parents of a merge commit. Ties between commits at the same height are broken
by timestamp (newer first), so the walk from main HEAD is:

```
hashM -> hashC -> hashB -> hashA -> init
```

(hashC is committed later than hashB, so it sorts before hashB under the
newer-timestamp tie-breaker.)

---

## Scenario 6: Merge commit appears in doltLog with parent1 and parent2

```js
mdb.getSiblingDB("logmerge@main").runCommand({ doltLog: 1, limit: 1 })
```

Expected:

```json
{
  "commits": [
    {
      "commitId": "<hashM>",
      "parent1": "<hashB>",
      "parent2": "<hashC>",
      "refs": ["HEAD", "main"],
      "message": "Merge branch 'feat' into 'main'",
      "timestamp": "<...>",
      "author": "<...>",
      "committer": "<...>",
      "committerTimestamp": "<...>"
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

## Scenario 7: doltLog from feature tip shows only feature branch history

Starting traversal at `hashC` reaches only commits reachable from that tip:
`hashC -> hashA -> init`. `hashB` (reachable only via main's tip) and `hashM`
(the merge commit) must **not** appear.

```js
mdb.getSiblingDB("logmerge@main").runCommand({ doltLog: 1, from: hashC })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hashC>", "parent1": "<hashA>", "message": "add-three-feat", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashA>", "parent1": "<initH>", "message": "add-one",        "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
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

## Scenario 7b: doltLog on the feat branch connection

Running `doltLog` against `logmerge@feat` must walk feat's HEAD, not main's.
The walk is `hashC -> hashA -> init`; `hashB` and `hashM` are unreachable from
feat's tip. The tip commit (`hashC`) carries both `"HEAD"` and `"feat"` refs.

```js
mdb.getSiblingDB("logmerge@feat").runCommand({ doltLog: 1 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hashC>", "refs": ["HEAD", "feat"], "parent1": "<hashA>", "message": "add-three-feat",    "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashA>",                            "parent1": "<initH>", "message": "add-one",           "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<initH>",                                                   "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- `commits[0].commitId` equals `hashC` (feat tip  -- **not** `hashM`, the merge on main)
- `commits[0].refs` contains `"HEAD"` and `"feat"` (connection branch decoration)
- `hashB` (main-only) and `hashM` (merge) do **not** appear
- Non-head commits have no `refs` field

---

## Scenario 8: full walk visits both parents of the merge commit

From main HEAD the walk reaches every commit transitively reachable via **both**
parents of `hashM`, ordered by commit height then newer-timestamp-first.

```js
mdb.getSiblingDB("logmerge@main").runCommand({ doltLog: 1 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hashM>", "parent1": "<hashB>", "parent2": "<hashC>", "message": "Merge branch 'feat' into 'main'", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashC>", "parent1": "<hashA>", "message": "add-three-feat", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashB>", "parent1": "<hashA>", "message": "add-two",        "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashA>", "parent1": "<initH>", "message": "add-one",        "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<initH>",                        "message": "Initialize database", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- All five commits appear (previously `hashC` was missing because the walk
  only followed `parent1`).
- `hashC` sorts before `hashB`  -- both have the same height (2), tied by
  newer timestamp first.

---

## Scenario 9: limit truncates the topological walk

`limit=2` from main HEAD returns the two highest-priority commits in topological
order. `hashC` still wins the height-2 tie over `hashB`, so it appears in the
result and `hashB` does not.

```js
mdb.getSiblingDB("logmerge@main").runCommand({ doltLog: 1, limit: 2 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<hashM>", "parent1": "<hashB>", "parent2": "<hashC>", "message": "Merge branch 'feat' into 'main'", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" },
    { "commitId": "<hashC>", "parent1": "<hashA>", "message": "add-three-feat", "timestamp": "<...>", "author": "<...>", "committer": "<...>", "committerTimestamp": "<...>" }
  ],
  "ok": 1
}
```

Key checks:
- Exactly 2 entries
- `commits[0]` is the merge commit `hashM` with both `parent1` and `parent2`
- `commits[1]` is `hashC`  -- newer timestamp wins the height-2 tie over `hashB`
- `hashA`, `hashB`, and `init` do **not** appear

---

## Scenario 10: stat flag -- per-collection change summary

The `stat` flag adds a `changes` array to each commit -- the same change-set
shape as `dumboStatus`, at summary verbosity (document/index counts). This is
analogous to `git log --stat`.

```js
db.runCommand({ doltLog: 1, limit: 1, stat: true })
```

Expected (commit "third" added one document to "events"):

```json
{
  "commits": [
    {
      "commitId": "<hash3>",
      "parent1": "<hash2>",
      "message": "third",
      "timestamp": "<...>",
      "author": "carol <carol@startup.dev>",
      "committer": "<...>",
      "committerTimestamp": "<...>",
      "changes": [
        {
          "type": "collection", "name": "events", "status": "modified",
          "documents": { "added": 1, "removed": 0, "modified": 0 },
          "indexes": { "added": [], "removed": [], "modified": [] },
          "metadata": {}
        }
      ]
    }
  ],
  "ok": 1
}
```

Key checks:
- `changes` is present on the commit entry (summary verbosity)
- `changes[0]` has `type: "collection"`, `name: "events"` (the only change)
- `changes[0].documents.added` is `1` (one document inserted in this commit)
- `changes[0].documents.modified` and `documents.removed` are `0`

---

## Scenario 11: patch flag -- full document diffs

The `patch` flag emits the same `changes` array at full verbosity: each
commit's `changes` carries the full document-level diff between the commit and
its first parent, matching `dumboDiff`. This is analogous to `git log --patch`.
(When both `stat` and `patch` are requested, `changes` is full.)

```js
db.runCommand({ doltLog: 1, limit: 1, patch: true })
```

Expected (commit "third" added `{_id: 3, label: "gamma"}` to "events"):

```json
{
  "commits": [
    {
      "commitId": "<hash3>",
      "parent1": "<hash2>",
      "message": "third",
      "timestamp": "<...>",
      "author": "carol <carol@startup.dev>",
      "committer": "<...>",
      "committerTimestamp": "<...>",
      "changes": [
        {
          "type": "collection", "name": "events", "status": "modified",
          "documents": {
            "added": [ { "_id": 3, "label": "gamma" } ],
            "removed": [],
            "modified": []
          },
          "indexes": { "added": [], "removed": [], "modified": [] },
          "metadata": {}
        }
      ]
    }
  ],
  "ok": 1
}
```

Key checks:
- `changes` is present at full verbosity
- `changes[0].name` is `"events"`
- `changes[0].documents.added` contains the document `{_id: 3, label: "gamma"}`
- `changes[0].documents.removed` and `documents.modified` are empty arrays

---

## Scenario 12: stat and patch surface index changes

At summary verbosity `changes[].indexes` lists index names; at full
verbosity it carries the full definition on each side. An index-only commit (no document changes) still appears in both
outputs.

Run this in a fresh database:

```js
var idb = db.getSiblingDB("logidxvdb")

idb.items.insertOne({ _id: 1, age: 30, name: "alpha" })
idb.runCommand({ doltCommit: 1, message: "seed", author: "alice <alice@acme.com>" })

// Index-only commit: no documents change, by_age is added.
idb.items.createIndex({ age: 1 }, { name: "by_age" })
idb.runCommand({ doltCommit: 1, message: "add by_age", author: "alice <alice@acme.com>" })

// stat shows indexes.added on the head commit. indexes.modified and
// indexes.removed are always present as empty arrays.
idb.runCommand({ doltLog: 1, limit: 1, stat: true })
// Expected: commits[0].changes[0] = {
//   type: "collection", name: "items", status: "modified",
//   documents: { added: 0, removed: 0, modified: 0 },
//   indexes: { added: [ "by_age" ], modified: [], removed: [] },
//   metadata: {}
// }

// patch shows the full index definition under indexes.added. All other
// change arrays (documents.added/removed/modified, indexes.modified/removed)
// are present as empty arrays.
idb.runCommand({ doltLog: 1, limit: 1, patch: true })
// Expected: commits[0].changes[0] = {
//   ...,
//   indexes: {
//     added: [ { name: "by_age", keys: [{ field: "age", direction: 1 }] } ],
//     modified: [], removed: []
//   }
// }
```

Key checks:
- `changes[0].indexes.added` contains `"by_age"` even though the
  `documents` counts are all `0`. `indexes.modified` and `indexes.removed`
  are present as empty arrays.
- At full verbosity, `changes[0].indexes.added` has one entry: full IndexInfo
  (name, keys with direction). `indexes.modified` and `indexes.removed` are
  present as empty arrays.
- The commit is NOT silently dropped from `changes`; before this fix, an
  index-only commit would not appear in patch output because the
  document-level diff was empty.

For the drop+recreate-with-different-spec case, summary lists the name in
`indexes.modified` and full verbosity shows a single `indexes.modified` entry
with both `from` and `to` (see `index-branch-isolation.md` Scenario 8).

---

## Scenario 13: stat and patch absent when not requested

When neither `stat` nor `patch` is passed, commit entries do not include
`stat` or `diff` fields.

```js
db.runCommand({ doltLog: 1, limit: 1 })
```

Key checks:
- `stat` field is absent from the commit entry
- `diff` field is absent from the commit entry

---

## Quick Reference

| Command | Behaviour |
|---|---|
| `{ doltLog: 1 }` | All commits from HEAD, up to default limit (20) |
| `{ doltLog: 1, limit: N }` (N > 0) | At most N commits from HEAD |
| `{ doltLog: 1, limit: 0 }` | Empty `commits` array |
| `{ doltLog: 1, from: "<hash>" }` | All commits from `<hash>` backwards |
| `{ doltLog: 1, from: "<hash>", limit: N }` | At most N commits from `<hash>` (empty when N=0) |
| `{ doltLog: 1, stat: true }` | Each commit gets a `changes` array at summary verbosity (counts) |
| `{ doltLog: 1, patch: true }` | Each commit gets a `changes` array at full verbosity (documents) |

- Commits are returned in reverse topological order  -- higher commits first,
  with ties broken by newer timestamp first. Both parents of merge commits are
  visited.
- Each entry contains `hash`, `message`, `timestamp`, `author`, `committer`, and `committerTimestamp`.
- `committer` equals `author` for regular commits and merges; for cherry-pick and rebase commits, `committer` is the person who applied the commit while `author` is preserved from the original.
- `parent1` is present on all non-root commits.
- `parent2` is present only on merge commits.
- The root commit has neither `parent1` nor `parent2`.
- When `stat: true`, each commit includes a `changes` array (the same unified shape as `doltDiff`) at summary verbosity: each entry is `{ type, name, status, documents, indexes, metadata }` with `documents` as counts, `indexes` as names, and `metadata` as the changed validator/option paths without their values.
- When `patch: true`, each commit includes the same `changes` array at full verbosity: `documents` carries full documents, `indexes` carries full definitions, and `metadata` carries the `from`/`to` values.
- `changes` is only present (with `stat`/`patch`) on commits that changed something relative to their first parent; a commit that changed only a collection's validator/options still appears, with a populated `metadata`.
