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

A bare push (`dumboPush {}`, no `to` and no `refSpec`) resolves its target from
the branch's **config** (see `branch.md`), which groups tracking by direction:

- `config.push` `{ remote, branch }` -- a persistent push target. When set, a
  bare push goes to `config.push.remote` / `config.push.branch`. This is a
  first-class, differently-named, possibly-triangular target that git cannot
  persist (git only has `branch.<name>.pushRemote`, a remote name with no branch).
- `config.pull` `{ remote, branch, rebase, ff }` -- the fetch upstream and its
  merge policy. When `config.push` is unset, a bare push falls back to
  `config.pull.remote` / `config.pull.branch` (the clone-then-push case).

Unlike git's `push.default=simple`, there is **no** same-named refusal: the push
target is always explicit (`config.push`) or defaulted (`config.pull`, then the
branch's own name), so a differently-named push is a normal persistent target
rather than an error. There is also **no** `git push -u`: a push never mutates
stored config. Set `config.pull` / `config.push` explicitly with
`dumboBranch { setConfig: ... }` (see `branch.md`).

Unlike git, a revision source may be pushed straight to a new branch
(`refSpec: "HEAD~1:older"`); git makes you fully qualify the destination as
`refs/heads/older`, but dumbo's remote side is always branches, so the
right-hand side is always a branch name.

Branch config is part of the branch interface: it is shown by `dumboBranch` (the
analog of `git branch -vv`) and set or cleared there too (see `branch.md`). The
`admin.system.branches` collection is internal storage, not the interface.

## Parameters

| Parameter | Type   | Required | Default              | Description                                                                 |
|-----------|--------|----------|----------------------|-----------------------------------------------------------------------------|
| `to`      | string | no\*     | `config.push`/`config.pull` remote | Remote to push to. Omit to resolve from branch config.        |
| `refSpec` | string | no       | connection branch    | git-style `[+]<source>[:<destination>]`. Omit for a bare push (`git push`). |
| `force`   | bool   | no       | `false`              | Overwrite a non-fast-forward remote (`git push --force`); same as a `+` prefix. |

\* Push needs a remote from somewhere: an explicit `to`, `config.push`, or
`config.pull`.

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
to. They use `/tmp/dumbo-remote-1` and `/tmp/dumbo-remote-2` (plus
`/tmp/dumbo-ff-remote` and `/tmp/dumbo-ff2-remote` in Scenarios 7 and 15). Remove
them first (`rm -rf /tmp/dumbo-remote-* /tmp/dumbo-ff*-remote`) so the pushes
start from empty remotes. `file://` behaves like every other transport for push.

---

## Setup: a database with one commit on main

```js
var db = db.getSiblingDB("pushvdb")

db.items.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ dumboCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
printjson(r1)
const hash1 = r1.commitId
print("hash1 =", hash1)

db.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-remote-1" })
db.runCommand({ dumboRemote: 1, action: "add", name: "origin2", url: "file:///tmp/dumbo-remote-2" })
```

`pushvdb` now has one commit on `main` (`hash1`, the HEAD) and two remotes.

---

## Scenario 1: Push a branch (`git push origin main`)

A colon-less branch refspec pushes it and records **no** config.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
```

Expected (no `commitBefore` -- this push creates `main` on the remote):

```json
{ "remote": "origin", "branch": "main", "commitPushed": "<hash1>", "upToDate": false, "ok": 1 }
```

Confirm no config was recorded:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: the "main" entry has NO "config" field.
```

---

## Scenario 2: A bare push with no remote and no config is an error (`git push`)

Without a refspec, an explicit `to`, or any branch config, a bare push cannot
resolve a target.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1 })
// Expected: ok: 0; errmsg says main has no push or pull config.
```

The remote is unchanged.

---

## Scenario 3: `setConfig` records `config.pull` (the fetch/push upstream)

Tracking is set explicitly through the branch interface -- there is no push `-u`.

```js
db.getSiblingDB("pushvdb@main").runCommand({
  dumboBranch: 1, branch: "main",
  setConfig: { pull: { remote: "origin", branch: "main" } }
})

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main carries config.pull { remote: "origin", branch: "main" }.
```

---

## Scenario 4: A bare push follows `config.pull` (`git push`)

With `config.pull` set and no `config.push`, a bare push falls back to the pull
upstream.

```js
var db = db.getSiblingDB("pushvdb")
db.items.insertOne({ _id: 2, label: "beta" })
db.runCommand({ dumboCommit: 1, message: "commit two", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1 })
// Expected: remote "origin", upToDate false, ok 1, and the before->after pair
// commitBefore: <hash1>, commitPushed: <hash2>.
```

---

## Scenario 5: An explicit push does not change stored config (`git push origin main`)

```js
var db = db.getSiblingDB("pushvdb")
db.items.insertOne({ _id: 3, label: "gamma" })
db.runCommand({ dumboCommit: 1, message: "commit three", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: config.pull is STILL { remote: "origin", branch: "main" } and there
// is no config.push -- an explicit push mutates nothing.
```

---

## Scenario 6: Re-push is idempotent

Scenario 5 just pushed the current HEAD to `origin`, so a bare push is a no-op.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1 })
// Expected: upToDate true, remote origin, ok 1.
```

---

## Scenario 7: Fast-forward-only by default; `force` overwrites (`git push --force`)

A push that is not a fast-forward of the remote is rejected unless `force` is
given. This is the real "someone pushed before you" case: two clones share a
common commit, both add their own commit, and the second push is refused because
the histories have diverged from the shared base.

```js
// Source db: one commit (c1), pushed to a fresh remote.
var s = db.getSiblingDB("pushffsrc")
s.items.insertOne({ _id: 1, who: "base" })
s.runCommand({ dumboCommit: 1, message: "c1", author: "a <a@a>" })
s.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-ff-remote" })
s.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })   // remote at c1

// A collaborator clones the remote -- it shares c1 as a common root and tracks origin.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-ff-remote", as: "pushffclone" })

// The source advances the remote first (c1 -> c2-source).
s.items.insertOne({ _id: 2, who: "source" })
s.runCommand({ dumboCommit: 1, message: "c2-source", author: "a <a@a>" })
s.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })   // remote now at c2-source

// The clone commits its own change on top of the shared c1 (c1 -> c2-clone).
var c = db.getSiblingDB("pushffclone")
c.items.insertOne({ _id: 3, who: "clone" })
c.runCommand({ dumboCommit: 1, message: "c2-clone", author: "b <b@b>" })

// The clone's push is a non-fast-forward: c2-clone and c2-source both descend
// from c1, so c2-clone is not a descendant of the remote's current head.
c.runCommand({ dumboPush: 1 })
// Expected: ok: 0 -- not a fast-forward.

// force overwrites the remote with the clone's history.
c.runCommand({ dumboPush: 1, force: true })
// Expected: ok: 1 -- the remote main now holds c2-clone (c2-source is discarded).
```

---

## Scenario 8: A new branch is pushed, then tracked via `setConfig`

Push a new branch explicitly, then record its upstream with `setConfig`.

```js
var db = db.getSiblingDB("pushvdb")
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1, branch: "release" })
db.getSiblingDB("pushvdb@release").runCommand({ dumboPush: 1, to: "origin", refSpec: "release" })
// Expected: ok 1, branch "release".

db.getSiblingDB("pushvdb@release").runCommand({
  dumboBranch: 1, branch: "release",
  setConfig: { pull: { remote: "origin", branch: "release" } }
})
db.getSiblingDB("pushvdb@release").runCommand({ dumboBranch: 1 })
// Expected: the "release" entry carries config.pull { remote: "origin", branch: "release" }.
```

---

## Scenario 9: Push a branch to a differently-named remote branch (refspec rename)

`refSpec: "main:published"` sends `main` to a different branch on the remote,
like `git push origin main:published`. An explicit refspec never changes config.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "main:published" })
```

Expected: the response echoes both the local and remote branch (main's HEAD is
already on the remote, so this creates `published` there without transferring
anything).

```json
{ "remote": "origin", "branch": "main", "remoteBranch": "published", "commitPushed": "<hash>", "upToDate": true, "ok": 1 }
```

Confirm config.pull is unchanged:

```js
db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main config.pull is STILL { remote: "origin", branch: "main" }.
```

---

## Scenario 10: An explicit remote with no matching config pushes same-named

A bare push with an explicit `to` a remote that main's config does **not**
target sends the branch to the same-named branch there, with no simple-mode
refusal, and leaves config untouched.

```js
var db = db.getSiblingDB("pushvdb")
db.runCommand({ dumboPush: 1, to: "origin2" })
// Expected: ok 1, remote "origin2", branch "main" -- main sent to origin2/main.

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: main config.pull is STILL { remote: "origin", branch: "main" } -- untouched.
```

---

## Scenario 11: `HEAD:<dst>` pushes the current head to a named branch

`HEAD` resolves to the connection branch's head; the destination is any branch.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD:handy" })
// Expected: ok 1, branch "main", remoteBranch "handy", commitPushed = main's head.
```

---

## Scenario 12: A revision source pushes an older commit (`HEAD~1:older`)

The left side may be a relative revision. dumbo pushes it straight to a branch --
git would require `refs/heads/older`.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD~1:older" })
// Expected: ok 1, remoteBranch "older", NO "branch" field (the source is not a
// branch), and commitPushed is the parent commit, not the current head.
```

---

## Scenario 13: A colon-less revision is an error

Without a `:dst`, a revision source has no branch name to push to.

```js
db.getSiblingDB("pushvdb").runCommand({ dumboPush: 1, to: "origin", refSpec: "HEAD~1" })
// Expected: ok: 0 -- names a commit, not a branch; use <source>:<branch>.
```

---

## Scenario 14: `config.push` is a persistent, differently-named push target

`config.push` records a triangular push target that survives across pushes --
the workflow git cannot persist. main fetches from `origin/main` (config.pull)
but pushes to `origin2/rev51` (config.push).

```js
var db = db.getSiblingDB("pushvdb")
db.getSiblingDB("pushvdb@main").runCommand({
  dumboBranch: 1, branch: "main",
  setConfig: { push: { remote: "origin2", branch: "rev51" } }
})

db.items.insertOne({ _id: 4, label: "delta" })
db.runCommand({ dumboCommit: 1, message: "commit four", author: "alice <alice@acme.com>" })
db.runCommand({ dumboPush: 1 })
// Expected: ok 1, remote "origin2", remoteBranch "rev51" -- a bare push follows config.push.

db.getSiblingDB("pushvdb@main").runCommand({ dumboBranch: 1 })
// Expected: config.pull is still { remote: "origin", branch: "main" } (the fetch
// upstream is untouched) and config.push is { remote: "origin2", branch: "rev51" }.
```

---

## Scenario 15: A leading `+` forces a non-fast-forward push

`+<source>` is the refspec equivalent of `force: true`. Repeat Scenario 7's
unrelated-history setup and force with a `+`.

```js
// (dbA has pushed main to /tmp/dumbo-ff2-remote; dbB has an unrelated history and the
//  same remote configured -- see Scenario 7.)
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
| `{ dumboPush: 1 }` (config.push set)                         | `git push` (to the push target)     |
| `{ dumboPush: 1 }` (only config.pull set)                    | `git push` (falls back to upstream) |
| `{ dumboPush: 1 }` (no config)                               | `git push` (errors -- no target)    |
| `{ dumboPush: 1, to: "origin2" }` (no matching config)       | `git push origin2` (same-named)     |
| `{ dumboPush: 1, to: "origin", refSpec: "+main" }`           | `git push --force origin main`      |

- A colon-less branch pushes it without changing config; a colon-less revision
  (no `:dst`) is an error.
- A bare push resolves its target from `config.push` (a persistent, possibly
  triangular target), else `config.pull`, else errors. There is no same-named
  refusal and no push `-u`.
- Branch config is shown and managed through `dumboBranch` (see `branch.md`),
  never by reading `admin.system.branches`.
- Pushes are fast-forward-only unless `force` (or a `+` prefix) is set.
- The destination is always a branch: a revision source (`HEAD~2:older`) pushes
  straight to a branch, no `refs/heads/` qualification needed.
