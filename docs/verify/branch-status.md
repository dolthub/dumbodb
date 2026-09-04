# dumboBranchStatus Verification

Manual verification guide for `dumboBranchStatus` end-to-end behavior. Work through
each scenario top to bottom. All scenarios share the single commit graph built in the
setup block; `dumboBranchStatus` is read-only, so the scenarios do not affect one
another.

> **Automated equivalent:** `tests/verify/branch_status_test.go`
> (`TestBranchStatusVerify`) covers every scenario in this document as sequential
> subtests using the same setup. Run it with:
> ```
> go test ./tests/... -run TestBranchStatusVerify -v
> ```

`dumboBranchStatus` reports, for each target refspec, how many commits it is `ahead`
and `behind` a base refspec. "Ahead" counts commits reachable from the target but not
the base; "behind" counts the reverse. Refspecs are branch names, tag names, commit
hashes, ancestor expressions (`main~2`, `b2^1`), or `HEAD`/`HEAD~N` (resolved against
the connection's branch). The aliases `doltBranchStatus` and `dumboBranchStatus` are
equivalent.

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Build a divergent commit graph

Run this once before the scenarios below. It reproduces this graph (time flows
left to right; each node is labeled with the branch whose HEAD it is, and `anc`
is the shared baseline commit):

```
          * b1 --- * b2
         /
* anc
         \
          * main --- * b3 --- * b4 --- * b5
```

```js
var db = db.getSiblingDB("bsdemo")

// Baseline commit "anc" on main.
db.seed.insertOne({ _id: 1 })
db.getSiblingDB("bsdemo").runCommand({ doltCommit: 1, message: "anc", author: "alice <alice@acme.com>" })

function emptyCommit(branch, msg) {
  return db.getSiblingDB("bsdemo@" + branch).runCommand(
    { doltCommit: 1, message: msg, author: "alice <alice@acme.com>", allowEmpty: true })
}
function makeBranch(from, name) {
  return db.getSiblingDB("bsdemo@" + from).runCommand({ doltBranch: 1, action: "add", branch: name })
}

makeBranch("main", "b1")   // b1 branches from anc
emptyCommit("main", "main")

emptyCommit("b1", "b1")
makeBranch("b1", "b2")
emptyCommit("b2", "b2")

makeBranch("main", "b3")
emptyCommit("b3", "b3")
makeBranch("b3", "b4")
emptyCommit("b4", "b4")
makeBranch("b4", "b5")
var b5 = emptyCommit("b5", "b5")
print("b5 head =", b5.commitId)

// Tags pointing at b1 and b5.
db.getSiblingDB("bsdemo").runCommand({ dumboTag: 1, name: "t1", hash: "b1" })
db.getSiblingDB("bsdemo").runCommand({ dumboTag: 1, name: "t5", hash: "b5" })
```

---

## Scenario 1: Ahead/behind across branches

Compare every branch against `main`.

```js
db.getSiblingDB("bsdemo@main").runCommand({
  dumboBranchStatus: 1,
  base: "main",
  targets: ["main", "b1", "b2", "b3", "b4", "b5"]
})
```

Expected (order matches the input `targets`):

| target | commitsAhead | commitsBehind |
|--------|--------------|---------------|
| main   | 0 | 0 |
| b1     | 1 | 1 |
| b2     | 2 | 1 |
| b3     | 1 | 0 |
| b4     | 2 | 0 |
| b5     | 3 | 0 |

Key checks:
- `base` is `{ target: "main", hash: "<32-char hash>" }`.
- Each `targets` entry carries `target` (string), `hash` (32-char string), and `commitsAhead`/`commitsBehind` (int32).
- A branch compared to itself (`main` vs `main`) is `0 / 0`.

---

## Scenario 2: Tags resolve like their target commits

`t1` points at `b1`, `t5` points at `b5`, so they report the same ahead/behind.

```js
db.getSiblingDB("bsdemo@main").runCommand({
  dumboBranchStatus: 1, base: "main", targets: ["t1", "t5"]
})
```

Expected: `t1` -> `1 / 1`, `t5` -> `3 / 0`. The `target` field echoes `"t1"` / `"t5"`
(the input refspec), while `hash` is the resolved commit.

---

## Scenario 3: HEAD and HEAD~N resolve against the connection's branch

`HEAD` and `HEAD~N` resolve relative to the branch in the connection's database name,
not `main`. Connect via `bsdemo@b5` and compare back to `main`.

```js
db.getSiblingDB("bsdemo@b5").runCommand({
  dumboBranchStatus: 1,
  base: "main",
  targets: ["HEAD", "HEAD~1", "HEAD~2"]
})
```

Expected: `HEAD` -> `3 / 0`, `HEAD~1` -> `2 / 0`, `HEAD~2` -> `1 / 0`.

Key check: the `target` field echoes `"HEAD"`, `"HEAD~1"`, `"HEAD~2"` verbatim, not the
rewritten branch name.

---

## Scenario 4: Single target string and commit hash

A single target may be passed as a string instead of an array; it is treated as a
one-element list. A bare commit hash is also a valid refspec.

```js
// Single string target.
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: "b5" })
// Expected: targets: [ { target: "b5", hash: "...", commitsAhead: 3, commitsBehind: 0 } ]

// Bare commit hash (use the b5 head printed during setup).
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: [b5.commitId] })
// Expected: one entry whose target and hash both equal b5.commitId, ahead 3, behind 0.
```

---

## Scenario 5: A merge commit counts all the commits it brings in

Merge `b2` into a fresh branch `rel` cut from `main`, then compare `rel` against
`main`. A merge commit is "ahead" by every commit reachable from it but not from the
base -- the merged-in branch commits **and** the merge commit itself, not just the
merge commit.

`b2` is `anc -> b1 -> b2`; `main` is `anc -> main`. Merging `b2` into `rel` (which
starts at `main`) gives:

```
          * b1 --- * b2
         /            \
* anc                  \
         \               \
          * main --- * rel (merge)   (rel is 3 ahead of main: b1, b2, and the merge)
```

```js
// Cut rel from main, then merge b2 into it.
db.getSiblingDB("bsdemo@main").runCommand({ doltBranch: 1, action: "add", branch: "rel" })
db.getSiblingDB("bsdemo@rel").runCommand({ doltMerge: 1, mergeIn: "b2" })

db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: ["rel"] })
```

Expected: `rel` -> `commitsAhead: 3, commitsBehind: 0`. The three commits ahead are
`b1`, `b2`, and the merge commit.

---

## Scenario 6: Errors

```js
// Missing targets -> error (targets is required).
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main" })
// Expected: command error (targets is a required field)

// Empty targets array -> error (at least one target required).
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: [] })
// Expected: command error (BadValue)

// Empty-string target -> error (not a valid refspec).
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: [""] })
// Expected: command error (BadValue)

// Unknown target -> error.
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, base: "main", targets: ["no-such-branch"] })
// Expected: command error

// Missing base -> error (base is required).
db.getSiblingDB("bsdemo@main").runCommand({ dumboBranchStatus: 1, targets: ["b1"] })
// Expected: command error
```

---

## Quick Reference

| Field | Type | Meaning |
|---|---|---|
| `base.target` | string | the base refspec, echoed verbatim |
| `base.hash` | string | resolved commit hash of the base |
| `targets[].target` | string | the target refspec, echoed verbatim (`HEAD~1` stays `HEAD~1`) |
| `targets[].hash` | string | resolved commit hash of the target |
| `targets[].commitsAhead` | int32 | commits reachable from the target but not the base |
| `targets[].commitsBehind` | int32 | commits reachable from the base but not the target |

- `base` and `targets` are both required; `targets` is an array of refspecs (or a
  single string) naming at least one target.
- A refspec is a commit hash, branch name, tag name, ancestor expression
  (`main~2`, `b2^1`), `HEAD`, or `HEAD~N`.
- Comparing a refspec to itself yields `0 / 0`.
- A merge commit is ahead by every commit it brings in (the merged-in branch
  commits plus the merge commit), not just the merge commit.
- Order of `targets` in the response matches the order of the request.
