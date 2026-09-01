# dumboFetch / dumboPull Verification

> **Automated equivalent:** `tests/verify/pull_test.go`
> Run with `go test ./tests/verify/ -run TestPullVerify -count=1 -timeout=5m`

Manual verification guide for `dumboFetch` and `dumboPull`. Work through each
scenario top to bottom; the setup below is shared.

These mirror git. `dumboFetch` is `git fetch`: it downloads every branch from a
remote into local tracking refs (`refs/remotes/<remote>/<branch>`) and touches no
local branch. `dumboPull` is `git pull` = fetch + merge: it fetches, then merges
the fetched commit for the current branch into that branch (fast-forward, a merge
commit, or a conflict). Both report the per-branch `commitBefore -> commit`
change, like git's `<before>..<after>`.

Upstream tracking drives the no-argument forms, exactly as in git: a bare
`dumboFetch` / `dumboPull` uses the current branch's upstream (see `push.md` and
`branch.md`). A freshly cloned database already tracks `origin` (see `clone.md`).

## Parameters

`dumboFetch`:

| Parameter | Type   | Required | Description                                             |
|-----------|--------|----------|---------------------------------------------------------|
| `from`    | string | no\*     | Remote to fetch. Omit to use the default branch upstream. |

`dumboPull`:

| Parameter | Type   | Required | Description                                              |
|-----------|--------|----------|----------------------------------------------------------|
| `from`    | string | no\*     | Remote to pull from. Omit to use the current branch upstream. |
| `ffOnly`  | bool   | no       | Fail if the pull is not a fast-forward (`git pull --ff-only`). |
| `noFF`    | bool   | no       | Always create a merge commit (`git pull --no-ff`).        |
| `message` | string | no       | Merge commit message.                                    |
| `author`  | string | no       | `Name <email>` for a merge commit.                       |

\* Required only when the branch has no upstream.

## Prerequisites

A running DumboDB instance and `mongosh`. Connect:

```js
mongosh mongodb://localhost:27017
```

Pick an empty/nonexistent path for `<HUB_DIR>` and substitute it below.

---

## Setup: a hub remote and a working clone

```js
var hub = db.getSiblingDB("hub")
hub.dropDatabase()
hub.items.insertOne({ _id: 1, v: "one" })
const h1 = hub.runCommand({ dumboCommit: 1, message: "c1", author: "alice <alice@acme.com>" }).commitId
hub.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file://<HUB_DIR>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

// A working clone that tracks origin/main.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<HUB_DIR>", as: "work" })
print("h1 =", h1)
```

---

## Scenario 1: dumboFetch updates tracking refs without moving local branches

Advance the hub, then fetch on the clone.

```js
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 2, v: "two" })
const h2 = hub.runCommand({ dumboCommit: 1, message: "c2", author: "alice <alice@acme.com>" }).commitId
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("work").runCommand({ dumboFetch: 1, from: "origin" })
```

Expected: the `main` entry reports `commitBefore: h1, commit: h2`.

```json
{ "remote": "origin", "branches": [ { "branch": "main", "commitBefore": "<h1>", "commit": "<h2>" } ], "ok": 1 }
```

But the local branch has not moved -- fetch never touches branch heads:

```js
db.getSiblingDB("work").items.countDocuments({})
// Expected: 1 -- still at h1; the tracking ref advanced, the branch did not.
```

---

## Scenario 2: dumboFetch with no remote uses the upstream

```js
db.getSiblingDB("work").runCommand({ dumboFetch: 1 })
// Expected: remote "origin" (from the default branch upstream); already up to date.
```

---

## Scenario 3: dumboPull fast-forwards the branch

The clone's `main` is at `h1` with `origin/main` fetched to `h2`. Pull fast-forwards.

```js
db.getSiblingDB("work@main").runCommand({ dumboPull: 1 })
```

Expected:

```json
{ "remote": "origin", "branch": "main", "commitBefore": "<h1>", "commitAfter": "<h2>", "fastForward": true, "alreadyUpToDate": false, "ok": 1 }
```

And the pulled data is now present:

```js
db.getSiblingDB("work").items.countDocuments({})
// Expected: 2
```

---

## Scenario 4: dumboPull with nothing new is up to date

```js
db.getSiblingDB("work@main").runCommand({ dumboPull: 1 })
// Expected: alreadyUpToDate: true, commitBefore == commitAfter, fastForward: false.
```

---

## Scenario 5: dumboPull with local and remote changes creates a merge commit

```js
// Local commit on the clone.
var w = db.getSiblingDB("work")
w.items.insertOne({ _id: 100, v: "local" })
w.runCommand({ dumboCommit: 1, message: "local change", author: "bob <bob@acme.com>" })

// Remote advances.
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 3, v: "three" })
hub.runCommand({ dumboCommit: 1, message: "c3", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("work@main").runCommand({ dumboPull: 1, message: "merge origin", author: "bob <bob@acme.com>" })
```

Expected: `fastForward: false`, `alreadyUpToDate: false`, and `commitAfter` is a
new merge commit (different from both `commitBefore` and the fetched commit). Both
sides' documents are present:

```js
db.getSiblingDB("work").items.countDocuments({})
// Expected: 4 (_id 1,2,3,100)
```

---

## Scenario 6: ffOnly rejects a non-fast-forward pull

Set up a fresh divergence and pull with `ffOnly`.

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<HUB_DIR>", as: "ffwork" })

// Local commit diverges from the remote's next commit.
var f = db.getSiblingDB("ffwork")
f.items.insertOne({ _id: 200, v: "ff-local" })
f.runCommand({ dumboCommit: 1, message: "ff local", author: "bob <bob@acme.com>" })

var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 4, v: "four" })
hub.runCommand({ dumboCommit: 1, message: "c4", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("ffwork@main").runCommand({ dumboPull: 1, ffOnly: true })
// Expected: ok: 0 -- not a fast-forward.
```

---

## Scenario 7: dumboPull with no upstream is an error

```js
var solo = db.getSiblingDB("solodb")
solo.dropDatabase()
solo.items.insertOne({ _id: 1 })
solo.runCommand({ dumboCommit: 1, message: "seed", author: "a <a@a>" })
db.getSiblingDB("solodb@main").runCommand({ dumboPull: 1 })
// Expected: ok: 0 -- the branch has no upstream; specify a remote with 'from'.
```

---

## Scenario 8: Conflicting pull reports conflicts

When both sides change the same document, the pull's merge conflicts. Like
`dumboMerge`, it returns per-collection conflict counts and leaves the branch
staged for resolution (resolve with `dumboResolveConflict` / `dumboMerge`).

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file://<HUB_DIR>", as: "cfwork" })

// Both sides edit _id:1 differently.
var c = db.getSiblingDB("cfwork")
c.items.updateOne({ _id: 1 }, { $set: { v: "clone-edit" } })
c.runCommand({ dumboCommit: 1, message: "clone edit", author: "bob <bob@acme.com>" })

var hub = db.getSiblingDB("hub")
hub.items.updateOne({ _id: 1 }, { $set: { v: "hub-edit" } })
hub.runCommand({ dumboCommit: 1, message: "hub edit", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("cfwork@main").runCommand({ dumboPull: 1, message: "pull", author: "bob <bob@acme.com>" })
// Expected: ok: 0; a "conflicts" array with the items collection and a count.
```

---

## Quick Reference

| Command                                                | git analog                     |
|--------------------------------------------------------|--------------------------------|
| `{ dumboFetch: 1, from: "origin" }`                    | `git fetch origin`             |
| `{ dumboFetch: 1 }`                                    | `git fetch`                    |
| `{ dumboPull: 1 }`                                      | `git pull`                     |
| `{ dumboPull: 1, from: "origin" }`                     | `git pull origin`              |
| `{ dumboPull: 1, ffOnly: true }`                       | `git pull --ff-only`           |
| `{ dumboPull: 1, noFF: true }`                         | `git pull --no-ff`             |

- `dumboFetch` updates every tracking ref and never moves a local branch.
- `dumboPull` fetches, then merges the fetched commit into the current branch:
  fast-forward, a merge commit, or a conflict (reported like `dumboMerge`).
- Both default their remote to the branch upstream; a bare form with no upstream
  errors.
