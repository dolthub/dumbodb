# dumboPush Verification

> **Automated equivalent:** `tests/verify/push_test.go`
> Run with `go test ./tests/verify/ -run TestPushVerify -count=1 -timeout=5m`

Manual verification guide for `dumboPush` end-to-end behavior. Work through each
scenario top to bottom; each builds on the previous setup.

`dumboPush` mirrors `git push`. A single `refSpec` option carries git's push
refspec, `[+]<source>[:<destination>]`:

- `<source>` is any revision -- a branch (`main`), `HEAD`, a relative revision
  (`HEAD~3`, `main~2`, `main^`), or a commit hash. `HEAD` and relative revisions
  resolve against the connection's branch.
- `<destination>` is the branch to update on the remote.
- A leading `+` forces a non-fast-forward update (same as `force: true`).

The behaviors below were checked against git 2.39.5 and match it, except where
noted:

- `refSpec: "main"` pushes `main` to `main` on the remote and does **not** change
  the branch's upstream (`git push origin main`).
- `refSpec: "HEAD:foo"` pushes the current head to branch `foo` on the remote.
- A bare push to the branch's **own** remote (its upstream, or `origin` by
  default) with no upstream errors, exactly like `git push origin`.
- A bare push to a **different** remote is a triangular push: it sends the branch
  to the same-named branch there and needs no upstream (`git push other-remote`).
- `setUpstream: true` records the upstream (`git push -u`); an explicit push
  never changes it.
- A bare push (`dumboPush {}`) follows the upstream; git's `simple` mode refuses
  it when the upstream's branch name differs from the local branch.

Unlike git, a revision source may be pushed straight to a new branch
(`refSpec: "HEAD~1:older"`); git makes you fully qualify the destination as
`refs/heads/older`, but dumbo's remote side is always branches, so the
right-hand side is always a branch name.

Upstream tracking is part of the branch interface: it is shown by `dumboBranch`
(the analog of `git branch -vv`) and set or cleared there too (see `branch.md`).
The `admin.system.branches` collection is internal storage, not the interface.

## Parameters

| Parameter     | Type   | Required | Default            | Description                                                          |
|---------------|--------|----------|--------------------|----------------------------------------------------------------------|
| `to`          | string | no\*     | branch's upstream  | Remote to push to. Omit to use the branch's upstream.                |
| `refSpec`     | string | no       | connection branch  | git-style `[+]<source>[:<destination>]`. Omit for a bare push (`git push`). |
| `force`       | bool   | no       | `false`            | Overwrite a non-fast-forward remote (`git push --force`); same as a `+` prefix. |
| `setUpstream` | bool   | no       | `false`            | Record the target as the source branch's upstream (`git push -u`).   |

\* Push needs a remote from somewhere: an explicit `to`, or the source branch's
upstream.

`refSpec` forms:

| refSpec              | Meaning                                                             |
|----------------------|--------------------------------------------------------------------|
| `main`               | push `main` to `main` (colon-less branch)                          |
| `HEAD`               | push the current head to a branch of the connection branch's name  |
| `main:published`     | push `main` to `published` on the remote                           |
| `HEAD:foo`           | push the current head to `foo`                                     |
| `HEAD~2:older`       | push a relative revision to `older`                                |
| `+main`              | force-push `main` to `main`                                        |

A colon-less source that is not a branch (e.g. `HEAD~2`) is an error: there is
no branch name to use for the destination.

## Response

```json
{ "remote": "<remote>", "branch": "<branch>", "remoteBranch": "<dst>", "commitBefore": "<hash>", "commitPushed": "<hash>", "upToDate": <bool>, "ok": 1 }
```

`branch` is the local branch pushed; it is **omitted** when the source is a
revision expression rather than a branch. `remoteBranch` is shown only when the
destination differs from `branch`. `commitBefore` is the remote branch's head
before the push and `commitPushed` is the commit now on it -- the analog of git's
`<before>..<after>` report. `commitBefore` is omitted when the push creates the
branch on the remote.

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

db.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<REMOTE_DIR>" })
db.runCommand({ dumboRemote: 1, action: "add", name: "origin2", url: "file://<REMOTE2_DIR>" })
```

`pushvdb` now has one commit on `main` (`hash1`, the HEAD) and two remotes.

---

## Scenario 1: Push a branch (`git push origin main`)

A colon-less branch refspec pushes it and does **not** set an upstream.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
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

## Scenario 2: Bare push to the own remote with no upstream is an error (`git push origin`)

Without a refspec and no upstream recorded, a push to the branch's own remote
(`origin` by default) errors, like `git push origin`.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin" })
// Expected: ok: 0; errmsg says main has no upstream.
```

The remote is unchanged.

---

## Scenario 3: `setUpstream` records the upstream (`git push -u origin main`)

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "main", setUpstream: true })
```

The push is up to date (Scenario 1 already sent `hash1`), but the upstream is now
recorded. Confirm through the branch interface:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main carries upstream { remote: "origin", ref: "main" }.
```

---

## Scenario 4: Bare push follows the upstream (`git push`)

With an upstream set, no `to` or `refSpec` is needed.

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
db.runCommand({ dumboPush: 1, to: "origin2", refSpec: "main", setUpstream: true })

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is now { remote: "origin2", ref: "main" }.
```

---

## Scenario 6: An explicit push does not change the upstream (`git push origin main`)

Pushing explicitly to `origin` while the upstream is `origin2` must leave the
upstream pointing at `origin2`.

```js
var db = db.getSiblingDB("pushvdb")
db.items.insertOne({ _id: 3, label: "gamma" })
db.runCommand({ dumboCommit: 1, message: "commit three", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })   // no setUpstream

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: upstream is STILL { remote: "origin2", ref: "main" } -- unchanged.
```

---

## Scenario 7: Re-push is idempotent

Scenario 6 just pushed the current HEAD to `origin`, so pushing it again is a
no-op.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
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
a.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

var b = db.getSiblingDB("pushffB")
b.dropDatabase()
b.items.insertOne({ _id: 1, who: "B" })
b.runCommand({ dumboCommit: 1, message: "B1", author: "b <b@b>" })
b.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<FF_REMOTE_DIR>" })

b.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
// Expected: ok: 0 -- not a fast-forward.

b.runCommand({ dumboPush: 1, to: "origin", refSpec: "main", force: true })
// Expected: ok: 1 -- the remote main now holds dbB's history.
```

---

## Scenario 9: A new branch at an existing commit is pushed and tracked

Pushing a branch whose tip is already on the remote still creates the remote
branch (no chunks transfer) and, with `setUpstream`, tracks it.

```js
var db = db.getSiblingDB("pushvdb")
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1, branch: "release" })
db.getSiblingDB("pushvdb@release").runCommand({ dumboPush: 1, to: "origin", refSpec: "release", setUpstream: true })
// Expected: ok 1, branch "release".

db.getSiblingDB("pushvdb@release").runCommand({ dumboBranch: 1 })
// Expected: the "release" entry carries upstream { remote: "origin", ref: "release" }.
```

---

## Scenario 10: Push a branch to a differently-named remote branch (refspec rename)

`refSpec: "main:published"` sends `main` to a different branch on the remote,
like `git push origin main:published`. With `setUpstream`, the branch then tracks
it -- its `upstream.ref` differs from the local name.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "main:published", setUpstream: true })
```

Expected: the response echoes both the local and remote branch (main's HEAD is
already on the remote, so this creates `published` there without transferring
anything).

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

A bare push to a remote the branch does **not** track sends the branch to the
same-named branch there and leaves the upstream untouched -- git's triangular
push. First re-point main's upstream at `origin/main`:

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main", setUpstream: true })
// Expected: main upstream is { remote: "origin", ref: "main" }.

db.runCommand({ dumboPush: 1, to: "origin2" })
// Expected: ok 1, remote "origin2", branch "main" -- main sent to origin2/main.

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main upstream is STILL { remote: "origin", ref: "main" } -- untouched.
```

---

## Scenario 12: `HEAD:<dst>` pushes the current head to a named branch

`HEAD` resolves to the connection branch's head; the destination is any branch.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD:handy" })
// Expected: ok 1, branch "main", remoteBranch "handy", commitPushed = main's head.
// No setUpstream, so main's upstream is unchanged.
```

---

## Scenario 13: A revision source pushes an older commit (`HEAD~1:older`)

The left side may be a relative revision. dumbo pushes it straight to a branch --
git would require `refs/heads/older`.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD~1:older" })
// Expected: ok 1, remoteBranch "older", NO "branch" field (the source is not a
// branch), and commitPushed is the parent commit, not the current head.
```

---

## Scenario 14: A colon-less revision is an error

Without a `:dst`, a revision source has no branch name to push to.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD~1" })
// Expected: ok: 0 -- names a commit, not a branch; use <source>:<branch>.
```

---

## Scenario 15: A bare push errors when the upstream name differs (git `simple`)

Set main to track a differently-named remote branch, then try a bare push.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main:renamed", setUpstream: true })
// main now tracks origin/renamed.

db.runCommand({ dumboPush: 1 })
// Expected: ok: 0 -- main's name does not match its upstream ref; push explicitly.
```

---

## Scenario 16: A leading `+` forces a non-fast-forward push

`+<source>` is the refspec equivalent of `force: true`. Repeat Scenario 8's
unrelated-history setup and force with a `+`.

```js
// (dbA has pushed main to <FF2_REMOTE_DIR>; dbB has an unrelated history and the
//  same remote configured -- see Scenario 8.)
b.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
// Expected: ok: 0 -- not a fast-forward.

b.runCommand({ dumboPush: 1, to: "origin", refSpec: "+main" })
// Expected: ok: 1 -- the '+' forces it, exactly like force: true.
```

---

## Quick Reference

| Command                                                       | git analog                          |
|---------------------------------------------------------------|-------------------------------------|
| `{ dumboPush: 1, to: "origin", refSpec: "main" }`            | `git push origin main`              |
| `{ dumboPush: 1, to: "origin", refSpec: "main:published" }`  | `git push origin main:published`    |
| `{ dumboPush: 1, to: "origin", refSpec: "HEAD:foo" }`        | `git push origin HEAD:foo`          |
| `{ dumboPush: 1, to: "origin", refSpec: "HEAD~2:older" }`    | `git push origin HEAD~2:refs/heads/older` |
| `{ dumboPush: 1, to: "origin", refSpec: "main", setUpstream: true }` | `git push -u origin main`    |
| `{ dumboPush: 1 }`                                            | `git push` (uses upstream)          |
| `{ dumboPush: 1, to: "origin" }` (own remote, no upstream)   | `git push origin` (errors)          |
| `{ dumboPush: 1, to: "origin2" }` (a remote it doesn't track)| `git push origin2` (triangular)     |
| `{ dumboPush: 1, to: "origin", refSpec: "+main" }`           | `git push --force origin main`      |

- A colon-less branch pushes it without changing tracking; a colon-less revision
  (no `:dst`) is an error.
- A bare push to the branch's own remote (its upstream, or `origin`) with no
  upstream errors; a bare push to a different remote is triangular and succeeds;
  a bare push whose upstream name differs from the branch errors (git `simple`).
- `setUpstream: true` records/overwrites the upstream; an explicit push never
  changes it.
- Upstream is shown and managed through `dumboBranch` (see `branch.md`), never by
  reading `admin.system.branches`.
- Pushes are fast-forward-only unless `force` (or a `+` prefix) is set.
- The destination is always a branch: a revision source (`HEAD~2:older`) pushes
  straight to a branch, no `refs/heads/` qualification needed.
