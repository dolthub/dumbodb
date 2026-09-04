# dumboFetch / dumboPull Verification

> **Automated equivalent:** `tests/verify/pull_test.go`
> Run with `go test ./tests/verify/ -run TestPullVerify -count=1 -timeout=5m`

Manual verification guide for `dumboFetch` and `dumboPull`. Work through each
scenario top to bottom; the setup below is shared.

These mirror git. `dumboFetch` is `git fetch`: it downloads every branch from a
remote into local tracking refs (`refs/remotes/<remote>/<branch>`) and touches no
local branch. Its `branches` result lists **only the tracking refs that actually
moved** (or were newly created), each as a `commitBefore -> commit` pair like
git's `<before>..<after>`; an up-to-date fetch reports an empty `branches`.
`dumboPull` is `git pull` = fetch + merge: it fetches, then merges the fetched
commit for the current branch into that branch (fast-forward, a merge commit, or
a conflict).

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
| `rebase`  | bool | no | Rebase the branch onto the fetched commit instead of merging (`git pull --rebase`). |
| `message` | string | no       | Merge commit message.                                    |
| `author`  | string | no       | `Name <email>` for a merge commit.                       |

\* Required only when the branch has no upstream.

A tracking branch may carry a persistent **pull policy** in `config.pull`
(`rebase`, `ff`) set via `dumboBranch` (see `branch.md`), the analog of git's
`branch.<name>.rebase` and `pull.ff`. `config.pull.branch` may name a
differently-named remote branch to merge (git's `branch.<name>.merge`). A bare
`dumboPull` honors the policy; passing `rebase` / `ffOnly` /
`noFF` explicitly overrides it for that call, exactly as git's command line beats
config.

## Prerequisites

A running DumboDB instance and `mongosh`. Connect:

```js
mongosh mongodb://localhost:27017
```

The scenarios use `/tmp/dumbo-hub` as the remote directory. Remove it first
(`rm -rf /tmp/dumbo-hub`) so the setup starts from an empty remote.

---

## Setup: a hub remote and a working clone

```js
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 1, v: "one" })
const h1 = hub.runCommand({ dumboCommit: 1, message: "c1", author: "alice <alice@acme.com>" }).commitId
hub.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-hub" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

// A working clone that tracks origin/main.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "work" })
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
// Expected: remote "origin" (from the default branch upstream). Scenario 1
// already fetched origin/main, so nothing moved: branches is [] (empty).
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

Confirm the tip is a real **merge commit** -- two parents (`parent1` is the
pre-pull head, `parent2` is the fetched commit):

```js
db.getSiblingDB("work@main").runCommand({ dumboLog: 1, limit: 1 }).commits[0]
// Expected: the tip has BOTH parent1 (== the earlier commitBefore) and parent2.
```

---

## Scenario 6: ffOnly rejects a non-fast-forward pull

Set up a fresh divergence and pull with `ffOnly`.

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "ffwork" })

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
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "cfwork" })

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

## Scenario 9: dumboPull with `noFF` forces a merge commit (`git pull --no-ff`)

Where Scenario 3 fast-forwards, `noFF` records a merge commit even when a
fast-forward was possible. Use a fresh clone with no local commits, advance the
hub, and pull with `noFF`.

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "nfwork" })

// The hub advances; the clone has no local commits, so a plain pull would
// fast-forward.
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 5, v: "five" })
const h5 = hub.runCommand({ dumboCommit: 1, message: "c5", author: "alice <alice@acme.com>" }).commitId
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("nfwork@main").runCommand({ dumboPull: 1, noFF: true, message: "merge origin (no-ff)", author: "bob <bob@acme.com>" })
```

Expected: `fastForward: false` and `alreadyUpToDate: false` even though a
fast-forward was possible, and `commitAfter` is a new merge commit -- distinct
from both `commitBefore` and the fetched commit `h5` (a plain pull here would
have set `commitAfter == h5`). The fetched document is present:

```js
db.getSiblingDB("nfwork").items.countDocuments({ _id: 5 })
// Expected: 1 -- c5 was merged in.
```

Confirm it is a real merge commit -- `parent2` is the fetched commit `h5`:

```js
db.getSiblingDB("nfwork@main").runCommand({ dumboLog: 1, limit: 1 }).commits[0]
// Expected: the tip carries parent2 == h5 (a plain fast-forward would have no parent2).
```

---

## Scenario 10: `dumboPull { rebase: true }` rebases instead of merging

With local and remote changes (as in Scenario 5), `rebase: true` replays the
local commit on top of the fetched commit -- a linear history, no merge commit
(`git pull --rebase`).

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "rbwork" })

// Local commit on the clone.
var r = db.getSiblingDB("rbwork")
r.items.insertOne({ _id: 300, v: "rb-local" })
r.runCommand({ dumboCommit: 1, message: "rb local", author: "bob <bob@acme.com>" })

// Remote advances.
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 6, v: "six" })
hub.runCommand({ dumboCommit: 1, message: "c6", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("rbwork@main").runCommand({ dumboPull: 1, rebase: true })
// Expected: rebased: true, fastForward: false, alreadyUpToDate: false.
```

Both the local (`_id 300`) and remote (`_id 6`) documents are present:

```js
db.getSiblingDB("rbwork").items.countDocuments({ _id: { $in: [300, 6] } })
// Expected: 2
```

The history is **linear** -- the local commit was replayed on top of the fetched
commit, so the tip has a `parent1` and **no** `parent2` (a merge would have set
one):

```js
db.getSiblingDB("rbwork@main").runCommand({ dumboLog: 1, limit: 1 }).commits[0]
// Expected: the tip has parent1 set and NO parent2 field.
```

---

## Scenario 11: A branch pull policy of rebase makes a bare pull rebase

Record `rebase` on the tracking branch, then a bare `dumboPull` rebases without
any per-call argument (`git config branch.main.rebase true; git pull`).

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "rbpol" })

db.getSiblingDB("rbpol@main").runCommand({ dumboBranch: 1, branch: "main", setConfig: { pull: { rebase: true } } })
// Expected: { branch: "main", config: { pull: { remote, branch, rebase: "true" } }, ok: 1 }

// Diverge (local + remote commit), then a BARE pull.
var r = db.getSiblingDB("rbpol")
r.items.insertOne({ _id: 301, v: "local" })
r.runCommand({ dumboCommit: 1, message: "local", author: "bob <bob@acme.com>" })
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 7, v: "seven" })
hub.runCommand({ dumboCommit: 1, message: "c7", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("rbpol@main").runCommand({ dumboPull: 1 })
// Expected: rebased: true -- the bare pull honored the branch policy.

db.getSiblingDB("rbpol@main").runCommand({ dumboLog: 1, limit: 1 }).commits[0]
// Expected: linear -- the tip has parent1 and NO parent2.
```

---

## Scenario 12: A branch pull policy of `ff: "only"`, and overriding it

Record `ff: "only"`; a bare pull then fails on a non-fast-forward (like
`pull.ff = only`). An explicit `noFF` overrides the policy for that call.

```js
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-hub", as: "ffpol" })
db.getSiblingDB("ffpol@main").runCommand({ dumboBranch: 1, branch: "main", setConfig: { pull: { ff: "only" } } })

// Diverge so the pull is not a fast-forward.
var r = db.getSiblingDB("ffpol")
r.items.insertOne({ _id: 302, v: "local" })
r.runCommand({ dumboCommit: 1, message: "local", author: "bob <bob@acme.com>" })
var hub = db.getSiblingDB("hub")
hub.items.insertOne({ _id: 8, v: "eight" })
hub.runCommand({ dumboCommit: 1, message: "c8", author: "alice <alice@acme.com>" })
hub.runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })

db.getSiblingDB("ffpol@main").runCommand({ dumboPull: 1 })
// Expected: ok: 0 -- the ff:only policy rejects a non-fast-forward.

db.getSiblingDB("ffpol@main").runCommand({ dumboPull: 1, noFF: true, message: "merge", author: "bob <bob@acme.com>" })
// Expected: ok: 1 -- an explicit noFF overrides the policy and merges.

db.getSiblingDB("ffpol@main").runCommand({ dumboLog: 1, limit: 1 }).commits[0]
// Expected: the override produced a merge commit -- the tip has a parent2.
```

---

## Scenario 13: A bare pull follows a differently-named upstream branch

A local branch may track an upstream whose name differs from its own -- local
`main` tracking `origin/trunk` (`config.pull.branch = "trunk"`, git's
`branch.<name>.merge`). A bare pull must resolve `refs/remotes/origin/trunk`, not
`refs/remotes/origin/main`. Use a fresh hub at `/tmp/dumbo-rn-hub` (remove it
first for a clean run).

```js
// Hub: c1 on main, a "trunk" branch off it; push both to the remote.
var h = db.getSiblingDB("rnhub")
h.items.insertOne({ _id: 1, v: "one" })
h.runCommand({ dumboCommit: 1, message: "c1", author: "alice <alice@acme.com>" })
h.runCommand({ dumboRemote: 1, action: "add", name: "origin", url: "file:///tmp/dumbo-rn-hub" })
db.getSiblingDB("rnhub@main").runCommand({ dumboBranch: 1, branch: "trunk" })
db.getSiblingDB("rnhub@main").runCommand({ dumboPush: 1, to: "origin", refSpec: "main" })
db.getSiblingDB("rnhub@trunk").runCommand({ dumboPush: 1, to: "origin", refSpec: "trunk" })

// Work: clone, then re-point local main at origin/trunk.
db.getSiblingDB("admin").runCommand({ dumboClone: 1, from: "file:///tmp/dumbo-rn-hub", as: "rnwork" })
db.getSiblingDB("rnwork@main").runCommand({ dumboBranch: 1, branch: "main", setConfig: { pull: { remote: "origin", branch: "trunk" } } })

// Advance origin/trunk with a new commit.
var ht = db.getSiblingDB("rnhub@trunk")
ht.items.insertOne({ _id: 2, v: "two" })
ht.runCommand({ dumboCommit: 1, message: "c2 on trunk", author: "alice <alice@acme.com>" })
db.getSiblingDB("rnhub@trunk").runCommand({ dumboPush: 1, to: "origin", refSpec: "trunk" })

// A bare pull on work main follows config.pull.branch=trunk.
db.getSiblingDB("rnwork@main").runCommand({ dumboPull: 1 })
// Expected: ok: 1, remote "origin", fastForward true -- main advanced to
// origin/trunk's commit (main now has both documents).
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
| `{ dumboPull: 1, rebase: true }`                       | `git pull --rebase`            |
| `{ dumboBranch: 1, branch: "main", setConfig: { pull: { rebase: true } } }` | `git config branch.main.rebase true` |
| `{ dumboBranch: 1, branch: "main", setConfig: { pull: { ff: "only" } } }`   | `git config pull.ff only`      |

- `dumboFetch` updates every tracking ref and never moves a local branch.
- `dumboPull` fetches, then merges (or, with `rebase`, rebases) the fetched commit
  into the current branch: fast-forward, a merge commit, a rebase, or a conflict
  (reported like `dumboMerge` / `dumboRebase`).
- A tracking branch's persistent pull policy (`rebase`, `ff`), set via
  `dumboBranch`, drives a bare `dumboPull`; explicit `rebase`/`noFF`/`ffOnly`
  override it for that call.
- Both default their remote to the branch upstream; a bare form with no upstream
  errors.
