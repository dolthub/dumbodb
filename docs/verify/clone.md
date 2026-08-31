# dumboClone Verification

> **Automated equivalent:** `tests/verify/clone_test.go`
> Run with `go test ./tests/verify/ -run TestCloneVerify -count=1 -timeout=5m`

Manual verification guide for `dumboClone` end-to-end behavior. Work through each
scenario top to bottom; the setup below is shared by all of them.

`dumboClone` mirrors `git clone`. It creates a new server-side database from a
remote, brings over every branch, and -- like git -- leaves the clone ready to
sync: it registers an `origin` remote pointing at the source and makes the
default branch track `origin/<default>`. After a clone, `dumboPush` /
`dumboFetch` with no target work against `origin`.

`dumboClone` is an admin command: run it against the `admin` database. This
guide uses `file://` remotes -- local directories the server can read and write
-- which behave like every other transport.

## Parameters

| Parameter | Type   | Required | Description                                            |
|-----------|--------|----------|--------------------------------------------------------|
| `from`    | string | yes      | Remote URL to clone (`git clone <url>`).               |
| `as`      | string | yes      | Name of the new database to create.                    |

## Response

```json
{ "db": "<as>", "from": "<url>", "defaultBranch": "<branch>", "commit": "<hash>", "branches": [ ... ], "ok": 1 }
```

## Prerequisites

A running DumboDB instance and `mongosh`. Connect:

```js
mongosh mongodb://localhost:27017
```

Pick two empty/nonexistent directory paths for `<SRC_DIR>` (the remote to clone)
and substitute them below.

---

## Setup: publish a source remote with two branches

Create a database, push `main`, then push a second branch, so `<SRC_DIR>` holds a
remote with two branches to clone.

```js
var src = db.getSiblingDB("srcdb")
src.dropDatabase()

src.items.insertOne({ _id: 1, label: "alpha" })
const r1 = src.runCommand({ dumboCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
const hash1 = r1.commitId

src.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<SRC_DIR>" })
src.runCommand({ dumboPush: 1, to: "origin", branch: "main" })

// A second branch at the same commit.
db.getSiblingDB("srcdb@main").runCommand({ dumboBranch: 1, branch: "feature" })
db.getSiblingDB("srcdb@feature").runCommand({ dumboPush: 1, to: "origin", branch: "feature" })

print("hash1 =", hash1)
```

`<SRC_DIR>` now has `main` and `feature`, both at `hash1`.

---

## Scenario 1: Clone a remote into a new database

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<SRC_DIR>", as: "clonedb" })
```

Expected:

```json
{ "db": "clonedb", "from": "file://<SRC_DIR>", "defaultBranch": "main", "commit": "<hash1>", "branches": [ "feature", "main" ], "ok": 1 }
```

The cloned data is readable:

```js
db.getSiblingDB("clonedb").items.findOne({ _id: 1 })
// Expected: { _id: 1, label: "alpha" }
```

Key checks:
- `defaultBranch` is `main`, `commit` is `hash1`
- `branches` contains both `main` and `feature`
- the seeded document is present in `clonedb`

---

## Scenario 2: Clone brings every branch

```js
db.getSiblingDB("clonedb@main").runCommand({ dumboBranch: 1 })
```

Expected: both branches exist locally, each at `hash1`.

```json
{
  "branches": [
    { "name": "feature", "commitId": "<hash1>", "current": false },
    { "name": "main",    "commitId": "<hash1>", "current": true,
      "upstream": { "remote": "origin", "ref": "main" } }
  ],
  "ok": 1
}
```

Key checks:
- both `main` and `feature` are present
- `main` (the default) carries an upstream (see Scenario 4); `feature` does not

---

## Scenario 3: Clone registers an origin remote (git clone parity)

The clone leaves an `origin` remote pointing at the source.

```js
db.getSiblingDB("clonedb").runCommand({ dumboRemote: 1, action: "list" })
```

Expected:

```json
{ "remotes": [ { "name": "origin", "url": "file://<SRC_DIR>" } ], "ok": 1 }
```

---

## Scenario 4: The default branch tracks origin, so a bare push works

`main` is set to track `origin/main`, so `dumboPush` needs no target -- exactly
like a freshly cloned git repo.

```js
db.getSiblingDB("clonedb@main").runCommand({ dumboBranch: 1 })
// Expected: main has upstream { remote: "origin", ref: "main" }.

// Add a commit and push with no target.
var c = db.getSiblingDB("clonedb")
c.items.insertOne({ _id: 2, label: "beta" })
c.runCommand({ dumboCommit: 1, message: "local change", author: "bob <bob@acme.com>" })
c.runCommand({ dumboPush: 1 })
// Expected: remote "origin", ok 1 -- the bare push follows the tracked upstream.
```

Key checks:
- `clonedb`'s `main` carries `upstream: { remote: "origin", ref: "main" }`
- `dumboPush` with no `to` succeeds and reports `remote: "origin"`

---

## Scenario 5: Cloning into an existing database is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<SRC_DIR>", as: "clonedb" })
// Expected: ok: 0; errmsg says the database already exists.
```

---

## Scenario 6: A reserved database name is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<SRC_DIR>", as: "admin" })
// Expected: ok: 0; errmsg says the name is reserved.
```

---

## Scenario 7: An unsupported remote scheme is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "ssh://host/org/repo", as: "nope" })
// Expected: ok: 0; errmsg says the scheme is unsupported.
```

---

## Quick Reference

| Command                                                                | Effect                                        |
|------------------------------------------------------------------------|-----------------------------------------------|
| `admin.runCommand({ dumboClone: 1, from: "<url>", as: "db" })`         | Clone `<url>` into a new database `db`         |

- `dumboClone` is an admin command; run it against the `admin` database.
- The clone brings every branch; the default branch is `main` when present.
- Like `git clone`, it registers an `origin` remote and makes the default branch
  track `origin/<default>`, so `push`/`fetch` with no target work afterward.
- Cloning into an existing database, a reserved name, or an unsupported scheme is
  rejected.
