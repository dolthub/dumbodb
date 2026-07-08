# dumboUndrop Verification

> **Automated equivalent:** `tests/verify/undrop_test.go`
> Run with `go test ./tests/verify/ -run TestUndropVerify -count=1 -timeout=5m`

Manual verification guide for the soft-delete / undrop feature end-to-end.
Work through each scenario top to bottom. Each section builds on the previous
setup.

`dropDatabase` does not delete data. It moves the database directory into the
preserved-drops directory at `<dataDir>/.dumbodb_dropped_databases/<name>/<dropId>/`, where
`dropId` is the nanosecond timestamp of the drop. `dumboUndrop` restores a
**copy**: the drop stays preserved and listed, so it can be restored again (for
example under several names via `to_database`). Repeat drops of the same name are
all retained, distinguished by `dropId`. A background job runs hourly and
permanently deletes any preserved drop more than 30 days old, logging an INFO
line per deletion.

`dumboUndrop` is **admin-only**: it must be run against the `admin` database,
because it operates across the whole instance rather than on a single database.

## Parameters

### dumboUndrop

| Parameter | Type   | Required | Default | Description                                                                                  |
|-----------|--------|----------|---------|----------------------------------------------------------------------------------------------|
| `name`    | string | no       |  --      | Database to restore. Omit to list databases available to undrop. Must be a root name (no `@`). |
| `dropId`  | string | no       |  --      | Selects one drop when `name` has more than one preserved copy. Use the `dropId` from the list. |
| `to_database` | string | no   |  --      | Restore the drop under this name instead of its original. Requires `name`; must be a root name (no `@`) and not a system database (`admin`, `config`, `local`). |
| `purgeMatching` | object | no |  --      | Purge mode: permanently delete drops matching `{name, dropId, droppedBefore}` (AND; at least one required). Mutually exclusive with the restore parameters. |

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

The drop is a copy, so it stays in the list after restore:

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 })
// Expected: dropped still contains undropvdb
```

Key checks:
- Both documents are present after undrop.
- `doltLog` returns the same number of commits as before the drop.
- `undropvdb` is still listed as dropped (the drop was copied, not consumed).

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

Both drops are still preserved (restore copies, it does not consume):

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "ledger").length
// Expected: 2
```

Key checks:
- Two drops of `ledger` coexist with distinct `dropId`s.
- Undrop with no `dropId` restores the most recently dropped copy.
- Both drops remain listed after the restore.

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

All three drops remain preserved (restore copies, it does not consume):

```js
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "journal").length
// Expected: 3  (v1, v2, v3)
```

Key checks:
- The restored data is `v2`, proving selection is by `dropId`, not "most recent" or "oldest".
- All three drops (v1, v2, v3) are untouched and still listed.

---

## Scenario 4c: Restore under different names (to_database), repeatedly

Pass `to_database` to restore a drop under a new name. Because restore copies,
one drop can seed several independent live databases.

```js
var s = db.getSiblingDB("srcdb")
s.items.insertOne({ _id: 1, tag: "orig" })
s.runCommand({ doltCommit: 1, message: "s1", author: "a <a@a>" })
s.dropDatabase()

// First copy
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "srcdb", to_database: "destdb" })
// Expected: { undropped: "destdb", dropId: <id>, ok: 1 }
db.getSiblingDB("destdb").items.findOne({ _id: 1 }).tag
// Expected: "orig"

// srcdb was NOT consumed -- it is still listed as a drop
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1 }).dropped.map(d => d.name).includes("srcdb")
// Expected: true

// Second copy from the same drop, under another name
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "srcdb", to_database: "destdb2" })
db.getSiblingDB("destdb2").items.findOne({ _id: 1 }).tag
// Expected: "orig" -- an independent copy

// The copies are independent
db.getSiblingDB("destdb").items.updateOne({ _id: 1 }, { $set: { tag: "changed" } })
db.getSiblingDB("destdb2").items.findOne({ _id: 1 }).tag
// Expected: "orig" -- destdb2 is unaffected by writes to destdb
```

Key checks:
- `undropped` reports the new name.
- Each restore produces an independent live database with `srcdb`'s data.
- `srcdb` stays listed as a drop after each restore.

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

// to_database without name is an error
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, to_database: "somewhere" })
// Expected: ok: 0; errmsg contains "to_database requires name"

// to_database must be a root name
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ledger", to_database: "dest@main" })
// Expected: ok: 0; errmsg says to_database must be a root database

// cannot restore onto a reserved system database
db.getSiblingDB("admin").runCommand({ dumboUndrop: 1, name: "ledger", to_database: "config" })
// Expected: ok: 0; errmsg says cannot restore to system database
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

## Scenario 8: Purge drops early with purgeMatching

`purgeMatching` permanently removes drops before the 30-day GC. The filter fields
(`name`, `dropId`, `droppedBefore`) combine with AND; at least one is required.

```js
var admin = db.getSiblingDB("admin")

// Set up: drop "svc" twice
var s = db.getSiblingDB("svc"); s.items.insertOne({ _id: 1 }); s.runCommand({ doltCommit: 1, message: "c", author: "a <a@a>" }); s.dropDatabase()
s = db.getSiblingDB("svc"); s.items.insertOne({ _id: 2 }); s.runCommand({ doltCommit: 1, message: "c", author: "a <a@a>" }); s.dropDatabase()

// Purge one specific drop by dropId
var id = admin.runCommand({ dumboUndrop: 1 }).dropped.filter(d => d.name === "svc")[0].dropId
admin.runCommand({ dumboUndrop: 1, purgeMatching: { dropId: id } })
// { purged: [ { name: "svc", dropId: <id>, droppedAt: ISODate("...") } ], ok: 1 }

// Purge all remaining drops of a name
admin.runCommand({ dumboUndrop: 1, purgeMatching: { name: "svc" } })
// purges the rest of svc's drops

// Purge everything dropped before a cutoff (name + droppedBefore = AND)
admin.runCommand({ dumboUndrop: 1, purgeMatching: { name: "orders", droppedBefore: ISODate("2026-06-01") } })
```

Validation:

```js
admin.runCommand({ dumboUndrop: 1, purgeMatching: {} })
// Expected: ok: 0; errmsg "requires at least one of name, dropId, droppedBefore"

admin.runCommand({ dumboUndrop: 1, purgeMatching: { droppedAt: ISODate("2026-06-01") } })
// Expected: ok: 0; errmsg "unknown field droppedAt" (typo guard; the field is droppedBefore)

admin.runCommand({ dumboUndrop: 1, name: "svc", purgeMatching: { name: "svc" } })
// Expected: ok: 0; errmsg "purgeMatching cannot be combined with name"
```

Key checks:
- Each `purgeMatching` call returns the drops it removed in `purged`.
- An empty filter, an unknown field, or mixing with restore params all error.

---

## Scenario 9 (manual): Preserved drops survive a server restart

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
| `... runCommand({ dumboUndrop: 1, name: "x" })`                     | Restore `x` (the most recent drop)                |
| `... runCommand({ dumboUndrop: 1, name: "x", dropId: "<id>" })`     | Restore a specific drop of `x`                     |
| `... runCommand({ dumboUndrop: 1, name: "x", to_database: "y" })`   | Restore `x`'s drop under the name `y`             |
| `... runCommand({ dumboUndrop: 1, purgeMatching: { name: "x" } })`  | Permanently delete drops matching the filter      |

- `dropDatabase` only works on a root database name; system databases (`admin`, `config`, `local`) cannot be dropped.
- `dumboUndrop` must be run against `admin`.
- Repeat drops of one name are all retained; `dropId` selects among them.
- `to_database` restores under a new name; it requires `name`.
- Undrop restores the complete commit history, not just the latest data.
- Undrop copies the drop; it stays listed and can be restored again until purged. Restoring onto a live database name is rejected.
- `purgeMatching` deletes drops early: filter by `name`, `dropId`, and/or `droppedBefore` (AND); at least one is required.
- Preserved databases are permanently deleted by a background job once they are more than 30 days old (checked hourly).
