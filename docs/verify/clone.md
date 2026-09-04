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

| Parameter     | Type   | Required | Description                                                                 |
|---------------|--------|----------|-----------------------------------------------------------------------------|
| `from`        | string | yes      | Remote URL to clone (`git clone <url>`).                                     |
| `as`          | string | yes      | Name of the new database to create.                                         |
| `trackAsMain` | string | no       | Map this remote branch onto the clone's local `main` (for a remote whose default is not `main`). See Scenario 8. |

## Response

```json
{ "db": "<as>", "from": "<url>", "ok": 1 }
```

`db` echoes `as` and `from` is the resolved remote URL. The clone brings every
branch and sets the default branch to track `origin`; inspect the result with
`dumboBranch` (see `branch.md`), not the clone response.

## Prerequisites

A running DumboDB instance and `mongosh`. Connect:

```js
mongosh mongodb://localhost:27017
```

The scenarios use `/tmp/dumbo-src` as the source remote directory. Remove it
first (`rm -rf /tmp/dumbo-src`) so the setup starts from an empty remote.

---

## Setup: publish a source remote with two branches

Create a database, push `main`, then push a second branch, so `/tmp/dumbo-src` holds a
remote with two branches to clone.

```js
var src = db.getSiblingDB("srcdb")

src.items.insertOne({ _id: 1, label: "alpha" })
const r1 = src.runCommand({ dumboCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
const hash1 = r1.commitId

src.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-src" })
src.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

// A second branch at the same commit.
db.getSiblingDB("srcdb@main").runCommand({ dumboBranch: 1, action: "add", branch: "feature" })
db.getSiblingDB("srcdb@feature").runCommand({ dumboPush: 1, to: "origin", refSpec: "feature" })

print("hash1 =", hash1)
```

`/tmp/dumbo-src` now has `main` and `feature`, both at `hash1`.

---

## Scenario 1: Clone a remote into a new database

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-src", as: "clonedb" })
```

Expected:

```json
{ "db": "clonedb", "from": "file:///tmp/dumbo-src", "ok": 1 }
```

The cloned data is readable:

```js
db.getSiblingDB("clonedb").items.findOne({ _id: 1 })
// Expected: { _id: 1, label: "alpha" }
```

Key checks:
- `db` echoes `clonedb`, `from` echoes the source URL
- the seeded document is present in `clonedb` (branches are verified next)

---

## Scenario 2: Clone brings every branch

```js
db.getSiblingDB("clonedb@main").runCommand({ dumboBranch: 1, action: "list" })
```

Expected: both branches exist locally, each at `hash1`.

```json
{
  "branches": [
    { "name": "feature", "commitId": "<hash1>" },
    { "name": "main",    "commitId": "<hash1>", "current": true,
      "config": { "pull": { "remote": "origin", "branch": "main" } } }
  ],
  "ok": 1
}
```

Key checks:
- both `main` and `feature` are present
- `main` (the default) carries a `config.pull` upstream (see Scenario 4); `feature` does not

---

## Scenario 3: Clone registers an origin remote (git clone parity)

The clone leaves an `origin` remote pointing at the source.

```js
db.getSiblingDB("clonedb").runCommand({ dumboRemote: 1, action: "list" })
```

Expected:

```json
{ "remotes": [ { "name": "origin", "url": "file:///tmp/dumbo-src" } ], "ok": 1 }
```

---

## Scenario 4: The default branch tracks origin, so a bare push works

`main` is set to track `origin/main` via `config.pull`, so `dumboPush` needs no
target -- exactly like a freshly cloned git repo.

```js
db.getSiblingDB("clonedb@main").runCommand({ dumboBranch: 1, action: "list" })
// Expected: main has config.pull { remote: "origin", branch: "main" }.

// Add a commit and push with no target.
var c = db.getSiblingDB("clonedb")
c.items.insertOne({ _id: 2, label: "beta" })
c.runCommand({ dumboCommit: 1, message: "local change", author: "bob <bob@acme.com>" })
c.runCommand({ dumboPush: 1 })
// Expected: remote "origin", ok 1 -- the bare push falls back to config.pull.
```

Key checks:
- `clonedb`'s `main` carries `config.pull: { remote: "origin", branch: "main" }`
- `dumboPush` with no `to` succeeds and reports `remote: "origin"`

---

## Scenario 5: Cloning into an existing database is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-src", as: "clonedb" })
// Expected: ok: 0; errmsg says the database already exists.
```

---

## Scenario 6: A reserved database name is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-src", as: "admin" })
// Expected: ok: 0; errmsg says the name is reserved.
```

---

## Scenario 7: An unsupported remote scheme is rejected

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "ssh://host/org/repo", as: "nope" })
// Expected: ok: 0; errmsg says the scheme is unsupported.
```

---

## Scenario 8: A remote with no `main` -- rejected, or mapped with `trackAsMain`

Every database must have a `main`. A remote that lacks one cannot be cloned
plainly; pass `trackAsMain` to map one of its branches onto the local `main`.
Push `main` to a differently-named branch on a fresh remote so it holds only
`release`:

```js
var s = db.getSiblingDB("srcnomain")
s.items.insertOne({ _id: 1, v: "rel" })
s.runCommand({ dumboCommit: 1, message: "c1", author: "a <a@a>" })
s.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-nomain" })
s.runCommand({ dumboPush: 1, to: "origin", refSpec: "main:release" })   // remote has only "release"

// Plain clone is rejected -- no main.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-nomain", as: "nomainclone" })
// Expected: ok: 0 -- remote has no "main" branch; clone with trackAsMain instead.

// trackAsMain maps the remote's release branch onto the clone's local main.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-nomain", as: "mapped", trackAsMain: "release" })
// Expected: ok: 1.

db.getSiblingDB("mapped@main").runCommand({ dumboBranch: 1, action: "list" })
// Expected: local "main" (holding release's content) tracking origin/release
//   -- config.pull { remote: "origin", branch: "release" } -- plus the
//   remote-tracking "origin/release". There is NO separate local "release"
//   branch (main only).
```

A `trackAsMain` that names a branch the remote does not have is rejected.

---

## Scenario 9: `trackAsMain` overrides the remote's own `main`

When the remote already has a `main`, `trackAsMain` still wins: the named branch
becomes the local `main`, and the remote's own `main` survives only as the
`origin/main` tracking ref (no local `main` from it).

```js
// Remote with main (doc _id:1) and a feature branch ahead (doc _id:2).
var o = db.getSiblingDB("srcov")
o.items.insertOne({ _id: 1, v: "onmain" })
o.runCommand({ dumboCommit: 1, message: "c1", author: "a <a@a>" })
o.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-ov" })
o.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
db.getSiblingDB("srcov@main").runCommand({ dumboBranch: 1, action: "add", branch: "feature" })
var f = db.getSiblingDB("srcov@feature")
f.items.insertOne({ _id: 2, v: "onfeature" })
f.runCommand({ dumboCommit: 1, message: "c2", author: "a <a@a>" })
db.getSiblingDB("srcov@feature").runCommand({ dumboPush: 1, to: "origin", refSpec: "feature" })

db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-ov", as: "ovclone", trackAsMain: "feature" })

db.getSiblingDB("ovclone@main").runCommand({ dumboBranch: 1, action: "list" })
// Expected: local "main" holds feature's content (both _id:1 and _id:2) and
//   tracks origin/feature; origin/main and origin/feature both exist as tracking
//   refs; there is no separate local "feature" branch.
```

---

## Quick Reference

| Command                                                                            | Effect                                        |
|------------------------------------------------------------------------------------|-----------------------------------------------|
| `admin.runCommand({ dumboClone: 1, from: "<url>", as: "db" })`                      | Clone `<url>` into a new database `db`         |
| `admin.runCommand({ dumboClone: 1, from: "<url>", as: "db", trackAsMain: "<b>" })` | Clone, mapping remote branch `<b>` onto local `main` |

- `dumboClone` is an admin command; run it against the `admin` database.
- The clone brings every branch and registers an `origin` remote; local `main`
  tracks `origin/main` (or `origin/<trackAsMain>`), so `push`/`fetch` with no
  target work afterward.
- Every database must have a `main`: cloning a remote with no `main` is rejected
  unless `trackAsMain` maps another branch onto it. `trackAsMain` becomes local
  `main` (no separate local copy) and overrides the remote's own `main` if present.
- Cloning into an existing database, a reserved name, or an unsupported scheme is
  rejected.
