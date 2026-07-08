# dumboUndrop Verification

> **Automated equivalent:** `tests/verify/undrop_test.go`
> Run with `go test ./tests/verify/ -run TestUndropVerify -count=1 -timeout=5m`

Manual verification guide for the soft-delete / undrop feature end-to-end.
Work through each scenario top to bottom. Each section builds on the previous
setup.

`dropDatabase` does not delete data. It moves the database directory into the
preserved-drops directory at `<dataDir>/.dumbodb_dropped_databases/<name>/<dropId>/`, where
`dropId` is the nanosecond timestamp of the drop. `dumboUndrop` moves it back.
Repeat drops of the same name are all retained, distinguished by `dropId`.
A background job runs hourly and permanently deletes any preserved drop more
than 30 days old, logging an INFO line per deletion.

`dumboUndrop` is **admin-only**: it must be run against the `admin` database,
because it operates across the whole instance rather than on a single database.

## Parameters

### dumboUndrop

| Parameter | Type   | Required | Default | Description                                                                                  |
|-----------|--------|----------|---------|----------------------------------------------------------------------------------------------|
| `name`    | string | no       |  --      | Database to restore. Omit to list databases available to undrop. Must be a root name (no `@`). |
| `dropId`  | string | no       |  --      | Selects one drop when `name` has more than one preserved copy. Use the `dropId` from the list. |

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

Some scenarios inspect the data directory on disk; have a shell open at the
`--data-dir` the server was started with.

---

## Setup: Create a database with two commits

Run this once before the scenarios below.

Use a fresh DumboDB instance (empty `--data-dir`) so the preserved-drops
directory starts empty.

```js
var shop = db.getSiblingDB("undropvdb")

// Commit 1: one document
shop.items.insertOne({ _id: 1, label: "alpha" })
const r1 = shop.runCommand({ doltCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
printjson(r1)

// Commit 2: a second document
shop.items.insertOne({ _id: 2, label: "beta" })
const r2 = shop.runCommand({ doltCommit: 1, message: "commit two", author: "bob <bob@widgets.io>" })
printjson(r2)

// How many commits does main have right now?
db.getSiblingDB("undropvdb").runCommand({ doltLog: 1 }).commits.length
```

Note the commit count printed by the last line; call it `N`. The scenarios
below confirm undrop restores all `N` commits.

---

## Scenario 1: dropDatabase soft-deletes (does not destroy)

```js
db.getSiblingDB("undropvdb").dropDatabase()
```

Expected:

```json
{ "dropped": "undropvdb", "ok": 1 }
```

The database is gone from the live listing:

```js
db.adminCommand({ listDatabases: 1 }).databases.map(d => d.name)
// Expected: the array does NOT contain "undropvdb"
```

But on disk it has only moved, not vanished. In your shell:

```bash
ls <data-dir>/.dumbodb_dropped_databases/undropvdb/
# Expected: exactly one subdirectory, named with a long number (the dropId)
```

Key checks:
- `dropDatabase` returns `dropped: "undropvdb"`, `ok: 1`.
- `undropvdb` no longer appears in `listDatabases`.
- A single preserved copy exists under `.dumbodb_dropped_databases/undropvdb/`.

---

## Scenario 2: List databases available to undrop

`dumboUndrop` with no `name` lists every preserved drop, most recently
dropped first. Admin-only.

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 })
```

Expected:

```json
{
  "dropped": [
    { "name": "undropvdb", "dropId": "1775505756999075683", "droppedAt": ISODate("...") }
  ],
  "ok": 1
}
```

Key checks:
- `dropped` has one entry for `undropvdb`.
- The entry carries a string `dropId` and a `droppedAt` date.

---

## Scenario 3: Undrop restores data AND full history

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "undropvdb" })
```

Expected:

```json
{ "undropped": "undropvdb", "dropId": "1775505756999075683", "ok": 1 }
```

Confirm the data is back:

```js
db.getSiblingDB("undropvdb").items.find().toArray()
// Expected: both { _id: 1, label: "alpha" } and { _id: 2, label: "beta" }
```

Confirm the commit history is intact (compare with `N` from setup):

```js
db.getSiblingDB("undropvdb").runCommand({ doltLog: 1 }).commits.length
// Expected: equals N -- every commit is restored, not just the latest data
```

The preserved-drops list is now empty for this name:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 })
// Expected: dropped no longer contains undropvdb
```

Key checks:
- Both documents are present after undrop.
- `doltLog` returns the same number of commits as before the drop.
- `undropvdb` is no longer listed as dropped.

---

## Scenario 4: Repeat drops are all kept; no dropId restores the most recent

Drops are never overwritten. Dropping the same name twice keeps both copies.

```js
// First generation
var g = db.getSiblingDB("ledger")
g.items.insertOne({ _id: 1, gen: "first" })
g.runCommand({ doltCommit: 1, message: "g1", author: "a <a@a>" })
g.dropDatabase()

// Second generation, same name, different data
g = db.getSiblingDB("ledger")
g.items.insertOne({ _id: 1, gen: "second" })
g.runCommand({ doltCommit: 1, message: "g2", author: "a <a@a>" })
g.dropDatabase()

// Two preserved copies now exist
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "ledger")
// Expected: two entries, each with a distinct dropId (most recent first)
```

With no `dropId`, undrop restores the **most recent** drop:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ledger" })
// Expected: { undropped: "ledger", dropId: <most recent>, ok: 1 }

db.getSiblingDB("ledger").items.findOne({ _id: 1 }).gen
// Expected: "second" -- the most recent copy was restored
```

The older copy is still preserved:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "ledger").length
// Expected: 1
```

Key checks:
- Two drops of `ledger` coexist with distinct `dropId`s.
- Undrop with no `dropId` restores the most recently dropped copy.

---

## Scenario 4b: Restore a specific (non-latest) drop by dropId

Passing an explicit `dropId` restores exactly that copy, regardless of how
recent it is. Here three drops of `journal` exist at once, and we restore the
**middle** one -- neither the newest nor the oldest.

```js
// Three generations, all dropped
var j = db.getSiblingDB("journal")
j.items.insertOne({ _id: 1, gen: "v1" }); j.runCommand({ doltCommit: 1, message: "j1", author: "a <a@a>" }); j.dropDatabase()
j = db.getSiblingDB("journal")
j.items.insertOne({ _id: 1, gen: "v2" }); j.runCommand({ doltCommit: 1, message: "j2", author: "a <a@a>" }); j.dropDatabase()
j = db.getSiblingDB("journal")
j.items.insertOne({ _id: 1, gen: "v3" }); j.runCommand({ doltCommit: 1, message: "j3", author: "a <a@a>" }); j.dropDatabase()

// List is most-recent-first: [v3, v2, v1]. The middle entry is v2.
var drops = db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "journal")
var middle = drops[1].dropId

db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "journal", dropId: middle })
// Expected: { undropped: "journal", dropId: <middle>, ok: 1 }

db.getSiblingDB("journal").items.findOne({ _id: 1 }).gen
// Expected: "v2" -- the specific, non-latest copy was restored
```

The other two copies remain preserved:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "journal").length
// Expected: 2  (v1 and v3)
```

Key checks:
- The restored data is `v2`, proving selection is by `dropId`, not "most recent" or "oldest".
- The other two drops (v1, v3) are untouched and still listed.

---

## Scenario 5: Error cases

```js
// Nothing to undrop under that name
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ghost" })
// Expected: ok: 0; errmsg contains "no dropped database"

// A live database with that name already exists
db.getSiblingDB("ledger")  // ledger is live again from Scenario 4
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ledger" })
// Expected: ok: 0; errmsg contains "already exists"

// Revision-qualified names are not allowed
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ledger@main" })
// Expected: ok: 0; errmsg says name must be a root database
```

Key check: each call returns an error; no database state changes.

---

## Scenario 6: undrop is admin-only

`dumboUndrop` operates across the whole instance, so it is rejected unless run
against the `admin` database.

```js
db.getSiblingDB("undropvdb").runCommand({ dumboUndrop: 1, name: "undropvdb" })
// Expected: ok: 0; errmsg contains "admin database"
```

Key check: undrop fails when not run against `admin`.

---

## Scenario 7: System databases cannot be dropped

`admin`, `config`, and `local` are protected and are never preserved.

```js
db.getSiblingDB("admin").dropDatabase()
// Expected: ok: 0; errmsg contains "system databases cannot be dropped"

db.getSiblingDB("config").dropDatabase()
// Expected: ok: 0; same error

db.getSiblingDB("local").dropDatabase()
// Expected: ok: 0; same error
```

Key check: all three error; the `admin` database remains fully usable.

---

## Scenario 8 (manual): Preserved drops survive a server restart

Soft-deleted databases live on disk, so they remain undroppable after a
restart.

```js
// Drop and confirm it is preserved
db.getSiblingDB("survivor").items.insertOne({ _id: 1 })
db.getSiblingDB("survivor").runCommand({ doltCommit: 1, message: "c1", author: "a <a@a>" })
db.getSiblingDB("survivor").dropDatabase()
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.map(d => d.name)
// Expected: includes "survivor"
```

Now stop and restart the DumboDB server (e.g. `Ctrl-C`, then re-run the same
launch command pointing at the same data directory). Reconnect and re-list:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.map(d => d.name)
// Expected: still includes "survivor"

db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "survivor" })
// Expected: { undropped: "survivor", ... ok: 1 }
```

Key check: the dropped database is still undroppable after a restart, and the
restore succeeds.

---

## Quick Reference

| Command                                                              | Effect                                            |
|---------------------------------------------------------------------|---------------------------------------------------|
| `db.getSiblingDB("x").dropDatabase()`                               | Soft-delete `x` (preserves it for undrop)         |
| `db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 })`           | List databases available to undrop                |
| `... runCommand({ dumboUndrop: 1, name: "x" })`                     | Restore `x` (errors if `x` has multiple drops)    |
| `... runCommand({ dumboUndrop: 1, name: "x", dropId: "<id>" })`     | Restore a specific drop of `x`                     |

- `dropDatabase` only works on a root database name; system databases (`admin`, `config`, `local`) cannot be dropped.
- `dumboUndrop` must be run against `admin`.
- Repeat drops of one name are all retained; `dropId` selects among them.
- Undrop restores the complete commit history, not just the latest data.
- Preserved databases are permanently deleted by a background job once they are more than 30 days old (checked hourly).
