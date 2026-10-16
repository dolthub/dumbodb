# dumboBackup Verification

> **Automated equivalent:** `tests/verify/backup_test.go`
> Run with `go test ./tests/verify/ -run 'TestBackupVerify|TestBackupDisabledUnderAuth' -count=1 -timeout=5m`

Manual verification guide for `dumboBackup` end to end. Work through each
scenario top to bottom; each builds on the previous ones.

`dumboBackup` follows Dolt backups. A backup holds one database's whole root --
every branch, tag and working set -- so uncommitted changes survive a backup and
restore. A sync replaces the backup's root with the database's; the first sync
copies everything and later syncs copy only what is missing.

`dumboBackup` is **admin-only**: it runs against the `admin` database and names
the database it acts on with `db`. Like Dolt, a backup carries one database
only: users, roles, remotes, backups and branch config live in `admin` and are
not carried. The `admin` database itself can never be backed up or restored.

The scenarios use two servers: **A** holds the source database and **B** is the
server a backup is restored onto.

## Parameters

| Parameter | Type   | Required                        | Default | Description |
|-----------|--------|---------------------------------|---------|-------------|
| `action`  | string | yes                             | --      | `add`, `list`, `remove`, `sync`, `sync-url`, or `restore` |
| `db`      | string | all actions except `restore`    | --      | Database being backed up |
| `name`    | string | `add`, `remove`, `sync`         | --      | Backup name |
| `url`     | string | `add`, `sync-url`, `restore`    | --      | Backup URL; any remote URL scheme (see `docs/COMMANDS.md`) |
| `as`      | string | `restore`                       | --      | Name of the database to create |
| `force`   | bool   | no                              | `false` | `restore` only: replace an existing database named `as` |

## Prerequisites

Two DumboDB servers, without `--auth`, with separate empty data directories,
and `mongosh`. Remove any backups left by an earlier run.

```bash
rm -rf /tmp/dumbo-a /tmp/dumbo-b /tmp/dumbo-backup-*
dumbodb --data-dir /tmp/dumbo-a --port 27017 &   # server A
dumbodb --data-dir /tmp/dumbo-b --port 27018 &   # server B
```

Scenario 11 needs a third server started with `--auth`.

---

## Setup: a database with history, a branch, a tag and uncommitted work

On server A (`mongosh mongodb://localhost:27017`):

```js
var orders = db.getSiblingDB("orders")
orders.items.insertMany([{ _id: 1 }, { _id: 2 }])
orders.runCommand({ dumboCommit: 1, message: "first two" })
orders.runCommand({ dumboTag: 1, name: "v1" })
orders.runCommand({ dumboBranch: 1, action: "add", branch: "feature" })

var feature = db.getSiblingDB("orders@feature")
feature.items.insertOne({ _id: 10 })
feature.runCommand({ dumboCommit: 1, message: "feature work" })

orders.items.insertOne({ _id: 3 })   // left uncommitted
orders.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-backup-origin" })

db.getSiblingDB("inventory").stock.insertOne({ _id: 1 })

var adminA = db.getSiblingDB("admin")
```

`orders` has 3 documents on `main` (one uncommitted), a `feature` branch with 3
documents, a tag `v1` and a remote `origin`. `inventory` is a second database
used in Scenario 8.

---

## Scenario 1: add -- register a named backup

```js
adminA.runCommand({ dumboBackup: 1, action: "add", db: "orders", name: "nightly", url: "file:///tmp/dumbo-backup-nightly" })
```

Expected:

```json
{ "name": "nightly", "url": "file:///tmp/dumbo-backup-nightly", "ok": 1 }
```

Adding a second backup under the same name is rejected:

```js
adminA.runCommand({ dumboBackup: 1, action: "add", db: "orders", name: "nightly", url: "file:///tmp/dumbo-backup-other" })
// Expected: ok: 0; errmsg: dumboBackup add: backup "nightly" already exists for database "orders"
```

---

## Scenario 2: list -- show a database's backups

```js
adminA.runCommand({ dumboBackup: 1, action: "list", db: "orders" })
```

Expected:

```json
{ "backups": [ { "name": "nightly", "url": "file:///tmp/dumbo-backup-nightly" } ], "ok": 1 }
```

Backups are per database:

```js
adminA.runCommand({ dumboBackup: 1, action: "list", db: "inventory" })
// Expected: { backups: [], ok: 1 }
```

---

## Scenario 3: sync -- copy the database to its backup

```js
adminA.runCommand({ dumboBackup: 1, action: "sync", db: "orders", name: "nightly" })
```

Expected (`root` is the database's root hash; call it `root1`):

```json
{ "url": "file:///tmp/dumbo-backup-nightly", "root": "<root1>", "upToDate": false, "ok": 1 }
```

Nothing has changed, so a second sync has nothing to copy:

```js
adminA.runCommand({ dumboBackup: 1, action: "sync", db: "orders", name: "nightly" })
// Expected: { url: ..., root: "<root1>", upToDate: true, ok: 1 }
```

---

## Scenario 4: restore -- recreate the database on another server

On server B (`mongosh mongodb://localhost:27018`):

```js
var adminB = db.getSiblingDB("admin")
adminB.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-nightly", as: "orders" })
```

Expected (the same root the sync reported):

```json
{ "db": "orders", "url": "file:///tmp/dumbo-backup-nightly", "root": "<root1>", "ok": 1 }
```

Everything came across, including the uncommitted insert:

```js
db.getSiblingDB("orders").items.countDocuments()           // 3
db.getSiblingDB("orders@feature").items.countDocuments()   // 3
db.getSiblingDB("orders").runCommand({ dumboTag: 1 })      // one tag, v1
db.getSiblingDB("orders").runCommand({ dumboStatus: 1 })   // dirty: true
```

Config kept in `admin` did not:

```js
db.getSiblingDB("orders").runCommand({ dumboRemote: 1, action: "list" })
// Expected: { remotes: [], ok: 1 }
adminB.runCommand({ dumboBackup: 1, action: "list", db: "orders" })
// Expected: { backups: [], ok: 1 }
```

---

## Scenario 5: sync again -- only the change is copied

On server A:

```js
db.getSiblingDB("orders").items.insertOne({ _id: 4 })
adminA.runCommand({ dumboBackup: 1, action: "sync", db: "orders", name: "nightly" })
```

Expected: a new root (`root2`, different from `root1`):

```json
{ "url": "file:///tmp/dumbo-backup-nightly", "root": "<root2>", "upToDate": false, "ok": 1 }
```

---

## Scenario 6: restore over an existing database (force)

On server B, change the restored database, then restore over it:

```js
db.getSiblingDB("orders").items.insertOne({ _id: 99 })

adminB.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-nightly", as: "orders" })
// Expected: ok: 0; errmsg: dumboBackup restore: database "orders" already exists; use force to replace it
db.getSiblingDB("orders").items.countDocuments()   // 4 -- untouched

adminB.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-nightly", as: "orders", force: true })
// Expected: { db: "orders", url: ..., root: "<root2>", ok: 1 }
db.getSiblingDB("orders").items.countDocuments()   // 4 -- _id 1, 2, 3, 4; _id 99 is gone
```

The replaced database was dropped, not destroyed:

```js
adminB.runCommand({ dumboUndrop: 1 })
// Expected: "dropped" includes an entry with name "orders"
```

---

## Scenario 7: sync-url -- a one-off backup to any URL

On server A:

```js
adminA.runCommand({ dumboBackup: 1, action: "sync-url", db: "orders", url: "file:///tmp/dumbo-backup-adhoc" })
// Expected: { url: "file:///tmp/dumbo-backup-adhoc", root: "<root2>", upToDate: false, ok: 1 }
```

No backup was registered:

```js
adminA.runCommand({ dumboBackup: 1, action: "list", db: "orders" })
// Expected: only "nightly"
```

On server B:

```js
adminB.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-adhoc", as: "orders_adhoc" })
db.getSiblingDB("orders_adhoc").items.countDocuments()   // 4
```

---

## Scenario 8: a backup URL cannot be shared

A sync replaces the destination's root, so a backup URL may not also be a
remote or another database's backup. On server A:

```js
// A remote's URL cannot become a backup
adminA.runCommand({ dumboBackup: 1, action: "add", db: "orders", name: "bad", url: "file:///tmp/dumbo-backup-origin" })
// Expected: ok: 0; errmsg: dumboBackup add: url "file:///tmp/dumbo-backup-origin" is already used by remote "orders.origin"

// Another database cannot sync to orders' backup
adminA.runCommand({ dumboBackup: 1, action: "sync-url", db: "inventory", url: "file:///tmp/dumbo-backup-nightly" })
// Expected: ok: 0; errmsg: dumboBackup sync-url: url "file:///tmp/dumbo-backup-nightly" is already used by backup "orders.nightly"

// A backup's URL cannot become a remote
db.getSiblingDB("inventory").runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-backup-nightly" })
// Expected: ok: 0; errmsg: dumboRemote add: url "file:///tmp/dumbo-backup-nightly" is already used by backup "orders.nightly"

// Clone would register the backup as its origin remote, so it is refused too
adminA.runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-backup-nightly", as: "loot" })
// Expected: ok: 0; errmsg: dumboClone: url "file:///tmp/dumbo-backup-nightly" is already used by backup "orders.nightly"
// and no database named "loot" exists
```

A database may sync to its own backup's URL with `sync-url`:

```js
adminA.runCommand({ dumboBackup: 1, action: "sync-url", db: "orders", url: "file:///tmp/dumbo-backup-nightly" })
// Expected: { ..., upToDate: true, ok: 1 }
```

---

## Scenario 9: remove -- forget a backup, keep its data

On server A:

```js
adminA.runCommand({ dumboBackup: 1, action: "remove", db: "orders", name: "nightly" })
// Expected: { ok: 1 }
adminA.runCommand({ dumboBackup: 1, action: "list", db: "orders" })
// Expected: { backups: [], ok: 1 }
adminA.runCommand({ dumboBackup: 1, action: "remove", db: "orders", name: "nightly" })
// Expected: ok: 0; errmsg: dumboBackup remove: backup "nightly" not found for database "orders"
```

The data already synced is still there. On server B:

```js
adminB.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-nightly", as: "orders_archive" })
db.getSiblingDB("orders_archive").items.countDocuments()   // 4
```

---

## Scenario 10: Error cases

On server A. Each call returns an error and changes nothing.

```js
// Must run against admin
db.getSiblingDB("orders").runCommand({ dumboBackup: 1, action: "list", db: "orders" })
// Expected: OperationFailed; errmsg: dumboBackup: can only be run against the admin database

// admin is never backed up or restored into
adminA.runCommand({ dumboBackup: 1, action: "sync-url", db: "admin", url: "file:///tmp/dumbo-backup-admin" })
// Expected: OperationFailed; errmsg: dumboBackup: the "admin" database cannot be backed up
adminA.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-adhoc", as: "admin", force: true })
// Expected: OperationFailed; errmsg: dumboBackup restore: cannot restore into the "admin" database

// Unknown names
adminA.runCommand({ dumboBackup: 1, action: "sync", db: "orders", name: "nope" })
// Expected: OperationFailed; errmsg: dumboBackup sync: backup "nope" not found for database "orders"
adminA.runCommand({ dumboBackup: 1, action: "sync-url", db: "nosuchdb", url: "file:///tmp/dumbo-backup-none" })
// Expected: OperationFailed; errmsg: dumboBackup sync-url: database "nosuchdb" does not exist
// and no database named "nosuchdb" is created

// Nothing to restore
adminA.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-empty", as: "x" })
// Expected: OperationFailed; errmsg contains: dumboBackup restore: no backup found at
// (create the empty directory /tmp/dumbo-backup-empty first)

// Malformed commands
adminA.runCommand({ dumboBackup: 1, action: "bogus" })
// Expected: BadValue; errmsg: dumboBackup: action must be add, list, remove, sync, sync-url, or restore
adminA.runCommand({ dumboBackup: 1, action: "add", db: "orders", name: "x" })
// Expected: BadValue; errmsg: dumboBackup add: url is required
adminA.runCommand({ dumboBackup: 1, action: "restore", url: "file:///tmp/dumbo-backup-adhoc" })
// Expected: BadValue; errmsg: dumboBackup restore: as is required
adminA.runCommand({ dumboBackup: 1, action: "list", db: "orders", bogus: 1 })
// Expected: IDLUnknownField (40415)
```

---

## Scenario 11: refused under access control

Start a third server with `--auth`, create a `root` user and authenticate as it.
Every action, under both `dumboBackup` and the alias `doltBackup`, is refused:

```js
db.getSiblingDB("admin").runCommand({ dumboBackup: 1, action: "list", db: "orders" })
// Expected: Unauthorized; errmsg: doltBackup is disabled when access control is enabled: push, pull, fetch, clone and backup use the server's own credentials
```

Repeat for `add`, `remove`, `sync`, `sync-url` and `restore`: all return
`Unauthorized`.

---

## Quick Reference

| Command (against `admin`) | Effect |
|---------------------------|--------|
| `{ dumboBackup: 1, action: "add", db, name, url }` | Register a named backup for `db` |
| `{ dumboBackup: 1, action: "list", db }` | List `db`'s backups |
| `{ dumboBackup: 1, action: "remove", db, name }` | Forget a backup; its data stays at the URL |
| `{ dumboBackup: 1, action: "sync", db, name }` | Copy `db` to its named backup |
| `{ dumboBackup: 1, action: "sync-url", db, url }` | Copy `db` to any URL without registering it |
| `{ dumboBackup: 1, action: "restore", url, as, force? }` | Create database `as` from the backup at `url` |

- A backup carries every branch, tag and working set, so uncommitted changes are restored.
- Remotes, backups, users and branch config live in `admin` and are not carried.
- `restore` refuses an existing database unless `force` is set; `force` drops it first, so it can be undropped.
- A backup URL cannot be a remote's or another database's backup URL, and cannot be cloned from.
- `list`, `sync` and `sync-url` may run on a replica-set secondary; `add`, `remove` and `restore` may not.
- Every action is refused under `--auth`.
