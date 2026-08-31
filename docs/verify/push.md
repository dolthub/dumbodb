# dumboPush Verification

> **Automated equivalent:** `tests/verify/push_test.go`
> Run with `go test ./tests/verify/ -run TestPushVerify -count=1 -timeout=5m`

Manual verification guide for `dumboPush` end-to-end behavior. Work through each
scenario top to bottom; each builds on the previous setup.

`dumboPush` mirrors `git push`. The behaviors below were checked against git
2.39.5 and match it:

- Naming a branch pushes it (`git push origin main`) and does **not** change the
  branch's upstream.
- A bare push to the branch's **own** remote with no upstream errors:
  `dumboPush {to:"origin"}` untracked fails, exactly like `git push origin`. A
  branch's own remote is its upstream, defaulting to `origin` when untracked.
- A bare push to a **different** remote is a triangular push: it sends the branch
  to the same-named branch there and needs no upstream (`git push other-remote`).
- `setUpstream: true` records the upstream (`git push -u`); it overwrites any
  previous upstream.
- With an upstream set, `dumboPush {}` uses it (`git push`).
- An explicit push never changes the upstream, even one pointing at another remote.

Upstream tracking is part of the branch interface: it is shown by `dumboBranch`
(the analog of `git branch -vv`) and set or cleared there too (see `branch.md`).
The `admin.system.branches` collection is internal storage, not the interface.

DumboDB v0 pushes a branch to the **same-named** branch on the remote; it does
not support git's `local:remote` refspec rename, so `upstream.ref` always equals
the branch name.

## Parameters

| Parameter     | Type   | Required | Default            | Description                                                          |
|---------------|--------|----------|--------------------|----------------------------------------------------------------------|
| `to`           | string | no\*     | branch's upstream  | Remote to push to. Omit to use the branch's upstream.               |
| `branch`       | string | no       | connection branch  | Local branch to push (a git refspec's left-hand side).              |
| `remoteBranch` | string | no       | same as `branch`   | Destination branch on the remote (`git push origin branch:remoteBranch`). |
| `force`        | bool   | no       | `false`            | Overwrite a non-fast-forward remote (`git push --force`).           |
| `setUpstream`  | bool   | no       | `false`            | Record the target as the branch's upstream (`git push -u`).         |

\* Push needs a remote from somewhere: an explicit `to`, or the branch's upstream.

## Response

```json
{ "remote": "<remote>", "branch": "<branch>", "commitBefore": "<hash>", "commitPushed": "<hash>", "upToDate": <bool>, "ok": 1 }
```

`commitBefore` is the remote branch's head before the push and `commitPushed`
is the commit now on it -- the analog of git's `<before>..<after>` report.
`commitBefore` is omitted when the push creates the branch on the remote.

## Prerequisites

A running DumboDB instance and `mongosh`. Connect:

```js
mongosh mongodb://localhost:27017
```

Scenarios push to `file://` remotes -- local directories the server can write
to. Pick two empty/nonexistent paths and substitute them for `<REMOTE_DIR>` and
`<REMOTE2_DIR>` (e.g. `/tmp/dumbo-remote-1`, `/tmp/dumbo-remote-2`). `file://`
behaves like every other transport for push.

---

## Setup: a database with one commit on main

```js
var db = db.getSiblingDB("pushvdb")
db.dropDatabase()

db.items.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ dumboCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
printjson(r1)
const hash1 = r1.commitId
print("hash1 =", hash1)
```

`pushvdb` now has one commit on `main` (`hash1`, the HEAD).

---

## Scenario 1: Push a named branch (`git push origin main`)

Naming the branch pushes it and does **not** set an upstream.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<REMOTE_DIR>" })
db.runCommand({ dumboPush: 1, to: "origin", branch: "main" })
```

Expected (no `commitBefore` -- this push creates `main` on the remote):

```json
{ "remote": "origin", "branch": "main", "commitPushed": "<hash1>", "upToDate": false, "ok": 1 }
```

Confirm no upstream was set:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: the "main" entry has NO "upstream" field.
```

---

## Scenario 2: Bare push with no upstream is an error (`git push origin`)

Without a branch named and no upstream recorded, there is nothing git would
push -- it errors, and so does DumboDB.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin" })
// Expected: ok: 0; errmsg says main has no upstream -- name a branch or use setUpstream.
```

The remote is unchanged.

---

## Scenario 3: `setUpstream` records the upstream (`git push -u origin main`)

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", branch: "main", setUpstream: true })
```

The push is up to date (Scenario 1 already sent `hash1`), but the upstream is now
recorded. Confirm through the branch interface:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
```

Expected: `main` carries `upstream: { remote: "origin", ref: "main" }`.

```json
{
  "branches": [
    { "name": "main", "commitId": "<hash1>", "current": true,
      "upstream": { "remote": "origin", "ref": "main" } }
  ],
  "ok": 1
}
```

---

## Scenario 4: Bare push follows the upstream (`git push`)

With an upstream set, no `to` is needed.

```js
var db = db.getSiblingDB("pushvdb")
db.items.insertOne({ _id: 2, label: "beta" })
db.runCommand({ dumboCommit: 1, message: "commit two", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1 })
// Expected: remote "origin", upToDate false, ok 1, and the before->after pair
// commitBefore: <hash1>, commitPushed: <hash2>.
```

---

## Scenario 5: `setUpstream` to a second remote switches the upstream (`git push -u origin2 main`)

`-u` overwrites the upstream with the new target.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboRemote: 1, action: "add", name: "origin2", url: "file://<REMOTE2_DIR>" })
db.runCommand({ dumboPush: 1, to: "origin2", branch: "main", setUpstream: true })

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is now { remote: "origin2", ref: "main" }.
```

---

## Scenario 6: An explicit push does not change the upstream (`git push origin main`)

Pushing explicitly to `origin` while the upstream is `origin2` must leave the
upstream pointing at `origin2` (verified against git).

```js
var db = db.getSiblingDB("pushvdb")
db.items.insertOne({ _id: 3, label: "gamma" })
db.runCommand({ dumboCommit: 1, message: "commit three", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1, to: "origin", branch: "main" })   // no setUpstream

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: upstream is STILL { remote: "origin2", ref: "main" } -- unchanged.
```

---

## Scenario 7: Re-push is idempotent

Scenario 6 just pushed the current HEAD to `origin`, so pushing it again is a
no-op.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", branch: "main" })
// Expected: upToDate true, remote origin, ok 1.
```

---

## Scenario 8: Fast-forward-only by default; `force` overwrites (`git push --force`)

A push that is not a fast-forward of the remote is rejected unless `force` is
given. Two databases with unrelated histories push to one remote.

```js
var a = db.getSiblingDB("pushffA")
a.dropDatabase()
a.items.insertOne({ _id: 1, who: "A" })
a.runCommand({ dumboCommit: 1, message: "A1", author: "a <a@a>" })
a.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<FF_REMOTE_DIR>" })
a.runCommand({ dumboPush: 1, to: "origin", branch: "main" })

var b = db.getSiblingDB("pushffB")
b.dropDatabase()
b.items.insertOne({ _id: 1, who: "B" })
b.runCommand({ dumboCommit: 1, message: "B1", author: "b <b@b>" })
b.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<FF_REMOTE_DIR>" })

b.runCommand({ dumboPush: 1, to: "origin", branch: "main" })
// Expected: ok: 0 -- not a fast-forward.

b.runCommand({ dumboPush: 1, to: "origin", branch: "main", force: true })
// Expected: ok: 1 -- the remote main now holds dbB's history.
```

---

## Scenario 9: A new branch at an existing commit is pushed and tracked

Pushing a branch whose tip is already on the remote still creates the remote
branch (no chunks transfer) and, with `setUpstream`, tracks it.

```js
var db = db.getSiblingDB("pushvdb")
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1, branch: "release" })
db.getSiblingDB("pushvdb@release").runCommand({ dumboPush: 1, to: "origin", branch: "release", setUpstream: true })
// Expected: ok 1, branch "release".

db.getSiblingDB("pushvdb@release").runCommand({ dumboBranch: 1 })
// Expected: the "release" entry carries upstream { remote: "origin", ref: "release" }.
```

---

## Scenario 10: Push to a differently-named remote branch (refspec)

`remoteBranch` sends the local branch to a different branch on the remote, like
`git push origin main:published`. You do not have to track that branch. With
`setUpstream`, the branch then tracks it -- its `upstream.ref` differs from the
local name.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", branch: "main", remoteBranch: "published", setUpstream: true })
```

Expected: the response echoes both the local and remote branch (main's HEAD is
already on the remote, so this creates `published` there without transferring
anything -- `upToDate` is true and there is no `commitBefore`).

```json
{ "remote": "origin", "branch": "main", "remoteBranch": "published", "commitPushed": "<hash>", "upToDate": true, "ok": 1 }
```

Confirm the tracking now points at the renamed branch:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is { remote: "origin", ref: "published" }.
```

---

## Scenario 11: Bare push to a different remote is triangular (`git push origin2`)

A bare push (no branch, no `setUpstream`) to a remote the branch does **not**
track sends the branch to the same-named branch there and leaves the upstream
untouched -- git's "triangular" current-branch push. This differs from Scenario 2
only in the target: Scenario 2's `origin` is the branch's own remote, so it needs
an upstream; a different remote does not.

First put `main`'s upstream in a known place (`origin`):

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin", branch: "main", setUpstream: true })
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is { remote: "origin", ref: "main" }.
```

Now push bare to `origin2` (added in Scenario 5), which `main` does not track:

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin2" })
// Expected: ok 1, remote "origin2", branch "main" -- main was sent to origin2/main.
```

The upstream is unchanged -- the triangular push never touches it:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is STILL { remote: "origin", ref: "main" }.
```

---

## Quick Reference

| Command                                                       | git analog                          |
|---------------------------------------------------------------|-------------------------------------|
| `{ dumboPush: 1, to: "origin", branch: "main" }`             | `git push origin main`              |
| `{ dumboPush: 1, to: "origin", branch: "main", remoteBranch: "published" }` | `git push origin main:published` |
| `{ dumboPush: 1, to: "origin", branch: "main", setUpstream: true }` | `git push -u origin main`   |
| `{ dumboPush: 1 }`                                            | `git push` (uses upstream)          |
| `{ dumboPush: 1, to: "origin" }` (own remote, no upstream)   | `git push origin` (errors)          |
| `{ dumboPush: 1, to: "origin2" }` (a remote it doesn't track)| `git push origin2` (triangular)     |
| `{ dumboPush: 1, to: "origin", branch: "main", force: true }`| `git push --force origin main`      |

- Naming a branch pushes it without changing tracking.
- A bare push to the branch's own remote (its upstream, or `origin`) with no
  upstream errors; a bare push to a different remote is triangular and succeeds.
- `setUpstream: true` records/overwrites the upstream; an explicit push never
  changes it.
- Upstream is shown and managed through `dumboBranch` (see `branch.md`), never by
  reading `admin.system.branches`.
- Pushes are fast-forward-only unless `force` is set.
- v0 pushes to the same-named remote branch; `local:remote` rename is not
  supported, so `upstream.ref` equals the branch name.
