# DocuDolt Version Control Features: Research & Design Proposal

**Date:** 2026-03-30
**Bead:** do-m6au
**Author:** jasper (polecat)

---

## 1. How Dolt Works — Core Concepts

Dolt is "Git for SQL databases." It implements Git's version control model on top
of a content-addressed chunk store (Noms Block Store / NBS), extended with Prolly
Trees for efficient ordered-key storage and efficient O(log N) diffs.

### Storage Model

```
NBS chunk store (content-addressed by SHA-256)
  StoreRoot (STRT)
    refsAM: "refs/heads/main" → DCMT hash
             "workingSets/heads/main" → WRST hash
  
  Commit (DCMT)
    rootValue → RTVL hash     # full DB state at this commit
    parents   → [DCMT hash]   # DAG of commits
    meta      → {author, email, message, timestamp}
  
  RootValue (RTVL)
    tables → ADRM             # tableName → table prolly.Map root
  
  Table (prolly.Map)
    key:   primary key bytes
    value: row bytes (BSON for DocuDolt)
```

### Git-Equivalent Operations (CLI + SQL)

| Git operation       | Dolt CLI          | Dolt SQL                          |
|---------------------|-------------------|-----------------------------------|
| `git commit`        | `dolt commit`     | `CALL DOLT_COMMIT()`              |
| `git branch`        | `dolt branch`     | `SELECT * FROM dolt_branches`     |
| `git merge`         | `dolt merge`      | `CALL DOLT_MERGE()`               |
| `git log`           | `dolt log`        | `SELECT * FROM dolt_commits`      |
| `git diff`          | `dolt diff`       | `SELECT * FROM dolt_diff_$TABLE`  |
| `git blame`         | `dolt blame`      | `SELECT * FROM dolt_blame_$TABLE` |
| `git push`          | `dolt push`       | `CALL DOLT_PUSH()`                |
| `git pull`          | `dolt pull`       | `CALL DOLT_PULL()`                |
| `git tag`           | `dolt tag`        | `SELECT * FROM dolt_tags`         |
| `git stash`         | `dolt stash`      | `CALL DOLT_STASH()`               |
| Point-in-time read  | n/a               | `SELECT * FROM db/commit:table`   |

### Key Dolt Differentiators

1. **Cell-wise merges**: Dolt merges at the cell level, not the row level.
   If two branches modify different fields of the same row, both changes are
   accepted without conflict. Git-style 3-way merge, but applied to structured data.

2. **Prolly Trees enable efficient diffs**: Content-addressed B-tree variant where
   two trees sharing the same subtree hash skip that subtree entirely. Diffs are
   O(changes) not O(total data size).

3. **Staging area**: Dolt has HEAD (last commit), staged (to be committed), and
   working (current in-memory state). This three-tier model allows selective
   commits and partial staging.

4. **SQL system tables**: All version control state is queryable via SQL.
   `dolt_diff_$TABLE` returns individual row diffs with `diff_type` (added/removed/modified)
   and both `$TABLE_diff` columns (before/after values per field).

5. **History tables**: `dolt_history_$TABLE` returns every version of every row
   across all commits — full time-series of the table.

---

## 2. How Git Works — Core Concepts

Git is a content-addressed DAG of snapshots.

### Storage Model

```
Object store (content-addressed by SHA-1/SHA-256)
  commit object
    tree  → tree hash    # full repo snapshot
    parent → commit hash  # DAG
    author, committer, message

  tree object
    entries: [mode, name, hash]  # directory listing
    each entry → blob or nested tree

  blob object
    raw file bytes
```

### Key Git Concepts

1. **Content-addressed storage**: Objects identified by their content hash.
   Two identical files share one blob. Moving a file costs nothing in content.

2. **DAG of commits**: Commits form a directed acyclic graph. Branches are
   mutable pointers to commit hashes. Tags are immutable pointers.

3. **Refs and branches**: `refs/heads/main` is a file containing a commit hash.
   Branches are cheap — creating one is writing a 40-byte hash.

4. **Staging area (index)**: Three-tier model: HEAD, index (staged), working tree.
   Allows fine-grained control over what enters each commit.

5. **Merge strategies**:
   - Fast-forward: if target is ancestor of source, just move the pointer
   - Recursive 3-way: find common ancestor, merge both diffs
   - Octopus: N-parent merges (rare)

6. **Remotes**: Named URLs for other repos. `git push origin main` writes local
   commits to the remote. `git pull` fetches and merges. Protocol is simple
   (pack files over HTTP/SSH).

7. **What makes Git the standard**: The DAG model is mathematically elegant.
   All operations reduce to graph traversal. The content-addressed store means
   corruption is detectable. Remotes are first-class citizens. The workflow
   (branch → edit → commit → push → PR → merge) maps naturally to software
   development.

---

## 3. Core Architectural Principle

**DocuDolt is a presentation layer. Dolt does the work.**

This is a hard requirement, not a preference. Every version control feature in
DocuDolt must be backed directly by an existing Dolt abstraction. DocuDolt's job is to
translate MongoDB wire protocol commands into calls to Dolt's Go APIs, then
translate the results back into BSON responses. DocuDolt must not reimplement
history walking, diff computation, merge logic, or any other algorithm that Dolt
already provides.

**Dolt exposes its VC capabilities through Go abstraction methods**, not raw SQL
strings. The SQL engine (and system tables like `dolt_history_$TABLE`,
`dolt_blame_$TABLE`, `dolt_commits`) may power those abstractions internally, but
DocuDolt calls the Go API layer — the same interfaces the Dolt CLI and SQL engine
themselves use. Do not construct SQL query strings in DocuDolt VC handlers.

Dolt is purpose-built for this work and is extremely fast at it:
- Prolly Tree diff is O(changes), not O(data size) — large collections with few
  changes diff in microseconds.
- DAG history walk reuses content-addressed chunk caching — repeated traversals
  of the same subtrees are essentially free.
- Cell-wise merge is already implemented, tested, and battle-hardened.

When designing a DocuDolt VC feature, the question is always: **"Which Dolt Go API
already does this?"** — not "How do we implement it?" If no Dolt abstraction
exists, escalate to the Dolt team rather than building it in DocuDolt.

---

## 4. The DocuDolt Opportunity — What to Expose

DocuDolt sits at a unique intersection: MongoDB wire protocol (document store UX)
backed by Dolt storage (version-controlled prolly trees). This creates an
opportunity for a "version-controlled MongoDB" that no existing product offers.

### Current DocuDolt Versioning State

DocuDolt already implements these commands (all behind the `doltXxx` namespace):

| Command          | What it does                                    | Status      |
|------------------|-------------------------------------------------|-------------|
| `doltCommit`    | Commit working set with message                 | Implemented |
| `doltBranch`    | Create branch from current branch               | Implemented |
| `doltMerge`     | Merge branch into current                       | Implemented |
| `doltLog`       | Commit history (hash, parent, message, ts)      | Implemented |
| `doltDiff`      | Document-level diff between two states          | Implemented |
| `doltReset`     | Move HEAD (soft or hard)                        | Implemented |
| `doltStatus`    | Show uncommitted collection changes             | Implemented |

Branch access is via the `dbname__d_rootish` naming convention, where the
rootish resolves to a commit (see Section 6 for the full specification).

### What's Missing (Gap Analysis)

The following Git/Dolt capabilities don't have DocuDolt equivalents yet:

1. **Point-in-time reads**: No way to query a collection as of a specific commit
   without doing a hard reset. Users need `db.collection.find()` at a past state.

2. **Tag support**: No named immutable checkpoints. Useful for release tagging,
   snapshot marking, or labeling known-good states.

3. **Document history**: No "show me all versions of document _id=X across all
   commits." Requires traversing the commit DAG per document.

4. **Document blame**: No "which commit last modified this field?" Dolt has
   `dolt_blame_$TABLE` for this.

5. **Remote push/pull**: No cross-server synchronization. Can't clone a DocuDolt
   instance or publish a database to a hub.

6. **Conflict resolution**: `doltMerge` exists but no interface for inspecting
   or resolving merge conflicts when they occur.

7. **List branches**: No command to enumerate existing branches. Users must know
   branch names in advance.

8. **Delete branch**: No cleanup of merged branches.

9. **Stash**: No "save uncommitted changes, clean working set, restore later."

10. **Cherry-pick**: No "apply commit X from branch Y to current branch."

11. **Collection-level point-in-time reads**: No way to say "give me this
    collection's documents as they existed at timestamp T or commit H."

---

## 5. Suggested Feature List

Features ranked by **impact/feasibility**. For each: the MongoDB wire protocol
surface and the **existing Dolt SQL primitive** that backs it. DocuDolt translates;
Dolt executes. No VC logic should be reimplemented in DocuDolt.

---

### Priority 1 (High Impact, Straightforward)

#### P1-A: Point-in-Time Reads (rootish db suffix)

**What**: Read any collection as of any historical commit, tag, or relative
ancestor — without modifying state.

**Wire protocol**: Generalize the existing `dbname__d_branchname` convention so
that anything after `__d_` is a **rootish** — a string that resolves to a specific
commit in Dolt's DAG. This is a strict superset of the current branch-name
behaviour; existing connection strings are unaffected.

Supported rootish forms:

| Connection string | Resolves to | Writable? |
|---|---|---|
| `mydb` | default branch, working set | ✅ yes |
| `mydb__d_main` | branch `main` tip | ✅ yes |
| `mydb__d_feature-x` | branch `feature-x` tip | ✅ yes |
| `mydb__d_v1.0` | tag `v1.0` | ❌ read-only |
| `mydb__d_abc123` | bare commit hash | ❌ read-only |
| `mydb__d_main~1` | parent of `main` | ❌ read-only |
| `mydb__d_main~3` | 3rd ancestor of `main` | ❌ read-only |

**Write semantics**: writable if and only if the rootish is a bare branch name
(or omitted). Tags, hashes, and relative expressions are always read-only.
Writes to a read-only rootish return an explicit error.

**Explicitly not supported** (connection-string hostile — require local stateful
context or are ambiguous outside a working directory):
- `HEAD` and `HEAD`-relative forms (`HEAD~1`, `HEAD^`)
- Reflog syntax (`main@{yesterday}`, `@{5 minutes ago}`)
- Range syntax (`main..feature`, `main...feature`)
- Regex commit search (`:/fix bug`)
- Type dereferencing (`v1.0^{commit}`, `^{}`)

**Examples**:
```javascript
// Current working set (default)
db.users.find({active: true})

// Specific branch
db.getSiblingDB("mydb__d_feature-x").users.find({active: true})

// Tagged release snapshot
db.getSiblingDB("mydb__d_v1.0").users.find({active: true})

// Bare commit hash
db.getSiblingDB("mydb__d_abc123f").users.find({active: true})

// One commit behind main (parent)
db.getSiblingDB("mydb__d_main~1").users.find({active: true})
```

**Dolt primitive**: Parse the rootish out of the db name. Resolve it to a commit
hash via Dolt's ref resolution API (handles branch names, tag names, hashes, and
`~N` ancestor traversal). Load the RTVL at that hash. Iterate/query the
prolly.Map at that root — no writes, completely non-destructive.

**Feasibility**: High. The backend already has the mechanics to load a collection
by RTVL hash. It's a matter of generalising the rootish parsing in
`branchFromDBName` to resolve hashes, tags, and `~N` expressions in addition to
bare branch names.

---

#### P1-B: List Branches (`doltListBranches`)

**What**: Return all branches for a database.

**Wire protocol**:
```javascript
db.runCommand({doltListBranches: 1})
// Returns: {branches: [{name: "main", hash: "abc..."}, ...], ok: 1}
```

**Dolt primitive**: Read `STRT.refsAM`; enumerate all entries starting with
`refs/heads/`. Each entry yields a branch name and its HEAD commit hash.

**Feasibility**: Very high. The STRT refsAM is already read at startup.

---

#### P1-C: Tag Support (`doltTag`, `doltListTags`)

**What**: Create named immutable pointers to commits. Useful for release markers,
snapshot labels, and as readable aliases for commit hashes.

**Wire protocol**:
```javascript
db.runCommand({doltTag: 1, name: "v1.0", message: "production release"})
db.runCommand({doltListTags: 1})
// Returns: {tags: [{name: "v1.0", hash: "abc...", message: "..."}], ok: 1}
```

**Dolt primitive**: Tags are stored in `STRT.refsAM` as `refs/tags/<name>`.
Creating a tag writes a new entry to the refsAM pointing to the current HEAD
commit hash. Tags are readable in the same pass as branches.

**Point-in-time reads with tags**: Tags integrate naturally with P1-A — tag
names are valid rootish values:
```javascript
db.getSiblingDB("mydb__d_v1.0").users.find()  // reads at the v1.0 tag
```

**Feasibility**: High. Same infrastructure as branches; just a different ref prefix.

---

#### P1-D: Delete Branch (`doltDeleteBranch`)

**What**: Remove a branch from the refsAM after it's been merged.

**Wire protocol**:
```javascript
db.runCommand({doltDeleteBranch: 1, branch: "feature-x"})
```

**Dolt primitive**: Remove the `refs/heads/<branch>` and
`workingSets/heads/<branch>` entries from `STRT.refsAM`, write new STRT.

**Feasibility**: High. Straightforward NBS update.

---

#### P1-E: Document History (`doltDocHistory`)

**What**: Return all versions of a single document across all commits on a branch.

**Wire protocol**:
```javascript
db.runCommand({
  doltDocHistory: 1,
  collection: "users",
  _id: ObjectId("abc123"),
  limit: 20
})
// Returns: {history: [{hash: "...", timestamp: ISODate, doc: {...}}, ...], ok: 1}
```

**Dolt primitive**: Dolt exposes per-row history via Go abstraction methods on
the database/table interfaces (backed by `dolt_history_$TABLE` internally). DocuDolt
calls those methods and translates the resulting row iterator to a BSON cursor.

**Performance note**: Dolt's Prolly Tree history walk is O(changes to that row),
not O(all commits). This is Dolt's core design — DocuDolt gets it for free.

**Feasibility**: High. Pure translation: Dolt Go API call → BSON cursor response.

---

### Priority 2 (Medium Impact, Moderate Effort)

#### P2-A: Conflict Report (`doltConflicts`)

**What**: After a merge that produced conflicts, show the conflicting documents.

**Wire protocol**:
```javascript
db.getSiblingDB("mydb__d_main").runCommand({doltConflicts: 1, collection: "users"})
// Returns: {conflicts: [{_id: ..., base: {...}, ours: {...}, theirs: {...}}, ...], ok: 1}
```

**Dolt primitive**: During a merge, when two branches have modified the same
document's same fields to different values, a conflict is generated. The backend
needs to write conflict entries (base/ours/theirs triples) to a separate map.
`doltConflicts` reads that map.

**Resolution**: Add `doltResolveConflict` to accept one side or a custom document.

**Feasibility**: Medium. Requires conflict state to be stored (new BSON map per
conflicting collection). The merge logic needs to populate it.

---

#### P2-B: Collection Blame (`doltBlame`)

**What**: For each document in a collection, show which commit last modified it.

**Wire protocol**:
```javascript
db.runCommand({doltBlame: 1, collection: "users"})
// Returns: {blame: [{_id: ..., hash: "...", author: "...", timestamp: ISODate}, ...], ok: 1}
```

**Dolt primitive**: Dolt exposes blame via Go abstraction methods (backed by
`dolt_blame_$TABLE` internally). DocuDolt calls those methods and translates the
result iterator to a BSON cursor.

**Feasibility**: High. Pure translation: Dolt Go API call → BSON cursor response.

---

#### P2-C: Stash (`doltStash`, `doltStashPop`)

**What**: Save the current working set without committing, return to the last
committed state, restore the stashed state later.

**Wire protocol**:
```javascript
db.runCommand({doltStash: 1, message: "WIP: migrating schema"})
db.runCommand({doltStashPop: 1})
db.runCommand({doltStashList: 1})
```

**Dolt primitive**: Save the current WRST.working_root_addr as a separate
named entry in the STRT refsAM (e.g., `stash/0`, `stash/1`). Reset the
working set to HEAD's RTVL. Pop restores the saved addr and removes the stash entry.

**Feasibility**: Medium. No new chunk types needed; uses existing ADRM and WRST.

---

### Priority 3 (Lower Impact or Higher Effort)

#### P3-A: Remote Push/Pull (`doltPush`, `doltPull`)

**What**: Synchronize a DocuDolt database with a remote DocuDolt or DoltHub instance.

**Wire protocol**:
```javascript
db.runCommand({doltPush: 1, remote: "origin", branch: "main"})
db.runCommand({doltPull: 1, remote: "origin", branch: "main"})
db.runCommand({doltAddRemote: 1, name: "origin", url: "dolt://host:27017/dbname"})
```

**Dolt primitive**: Dolt's NBS implements a chunk sync protocol: serialize all
chunks reachable from a commit that the remote doesn't have (pack file), send
over the wire, update remote's refsAM. DocuDolt can reuse Dolt's existing push/pull
protocol since both use the same NBS format.

**Why high value**: Enables multi-server workflows. "Branch in dev, push to prod
when ready." DoltHub integration would allow public database sharing.

**Feasibility**: Low-medium. Significant networking layer to build. But the chunk
format is already Dolt-compatible — the protocol work is the main cost.

---

#### P3-B: Cherry-Pick (`doltCherryPick`)

**What**: Apply the changes introduced by a specific commit to the current branch.

**Wire protocol**:
```javascript
db.getSiblingDB("mydb__d_main").runCommand({doltCherryPick: 1, commit: "abc123"})
```

**Dolt primitive**: Compute the diff between `commit`'s parent and `commit`.
Apply that diff as a patch to the current working set. Create a new commit.

**Feasibility**: Medium. Requires the diff primitive (already implemented) plus
a patch-apply step. May produce conflicts if the target branch diverged significantly.

---

#### P3-C: Schema Evolution Tracking

**What**: When a collection's document shape changes (fields added/removed/renamed),
track these schema migrations in the commit history. Auto-generate migration notes.

**Wire protocol**:
```javascript
db.runCommand({doltSchemaHistory: 1, collection: "users"})
// Returns a diff of field presence/type across commits
```

**Dolt primitive**: Diff the field keys across documents in two commits of the same
collection. Summarize which fields appeared, disappeared, or changed type.

**Feasibility**: Low (requires heuristic field analysis; MongoDB is schemaless so
this is statistical not structural).

---

#### P3-D: Point-in-Time Aggregation Pipeline Stage (`$asOf`)

**What**: A MongoDB aggregation pipeline stage that reads from a historical snapshot
mid-pipeline, enabling joins between current and historical data.

**Wire protocol**:
```javascript
db.orders.aggregate([
  {$match: {status: "pending"}},
  {$lookup: {
    from: {collection: "products", asOf: "v1.0"},
    localField: "productId",
    foreignField: "_id",
    as: "historicalProduct"
  }}
])
```

**Dolt primitive**: During aggregation pipeline execution, when `asOf` is present
in a `$lookup`, resolve the commit hash (or tag), load the historical prolly.Map,
and use it as the join source.

**Feasibility**: Low. Requires deep integration into the aggregation pipeline.
Worth designing but not implementing soon.

---

## 5. Feature Ranking Summary

| Feature                       | Impact | Feasibility | Priority | Dolt Primitive          |
|-------------------------------|--------|-------------|----------|-------------------------|
| P1-A: Point-in-time reads     | High   | High        | **P1**   | RTVL load at hash       |
| P1-B: List branches           | High   | Very High   | **P1**   | refsAM enumeration      |
| P1-C: Tag support             | High   | High        | **P1**   | refs/tags/* in refsAM   |
| P1-D: Delete branch           | Medium | High        | **P1**   | refsAM update           |
| P1-E: Document history        | High   | Medium      | **P1**   | DAG walk + prolly hash  |
| P2-A: Conflict report/resolve | Medium | Medium      | **P2**   | Conflict map (new)      |
| P2-B: Collection blame        | Medium | Medium      | **P2**   | DAG walk (all docs)     |
| P2-C: Stash                   | Medium | Medium      | **P2**   | stash/* in refsAM       |
| P3-A: Remote push/pull        | High   | Low         | **P3**   | Dolt chunk sync protocol|
| P3-B: Cherry-pick             | Low    | Medium      | **P3**   | Diff + patch apply      |
| P3-C: Schema tracking         | Low    | Low         | **P3**   | Field key diff          |
| P3-D: $asOf aggregation stage | High   | Very Low    | **P3**   | Pipeline + RTVL lookup  |

---

## 6. Implementation Notes

### The `dbname__d_rootish` Convention

The `__d_` separator distinguishes branch-encoded names from plain database names
that happen to contain double underscores. Anything after `__d_` is a **rootish**:
a string that resolves to a specific commit in Dolt's DAG.

```
mydb                    → default branch, working set (writable)
mydb__d_main            → branch main tip             (writable)
mydb__d_feature-x       → branch feature-x tip        (writable)
mydb__d_v1.0            → tag v1.0                    (read-only)
mydb__d_abc123          → bare commit hash             (read-only)
mydb__d_main~1          → parent of main              (read-only)
mydb__d_main~3          → 3rd ancestor of main        (read-only)
```

**Parsing rule**: split on the first `__d_`. The right-hand side is the rootish.
If no `__d_` is present, use the default branch working set.

**Rootish resolution order** (mirrors Dolt/Git ref lookup):
1. Branch name (`refs/heads/<rootish>`)
2. Tag name (`refs/tags/<rootish>`)
3. Bare commit hash (full or unambiguous prefix)
4. Relative ancestor expression (`<branch>~<N>` — resolve branch first, then
   walk N first-parents up the commit DAG)

**Not supported** (connection-string hostile):
- `HEAD` and `HEAD`-relative forms (`HEAD~1`, `HEAD^`) — HEAD has no meaning
  outside a local working directory; the default branch serves this role
- Reflog syntax (`main@{yesterday}`, `@{5 minutes ago}`)
- Range syntax (`main..feature`, `main...feature`)
- Regex commit search (`:/fix bug`)
- Type dereferencing (`v1.0^{commit}`, `^{}`)
- `^N` caret parent selection (use `~N` instead for clarity)

**Write-safety rule**: a connection is writable if and only if the rootish is a
bare branch name (or omitted). Everything else is read-only. DocuDolt enforces this
at the handler layer — write commands (`insert`, `update`, `delete`, `drop`, etc.)
on a read-only rootish return an explicit `OperationFailed` error.

This is a parsing change in `branchFromDBName` plus a read-only flag threaded
through to write handlers. The backend RTVL/prolly.Map loading is unchanged.

### Conflict Model

Dolt already tracks merge conflicts (base/ours/theirs triples per conflicting row)
and exposes resolution through Go abstraction methods (backed internally by
`dolt_conflicts_$TABLE` and `DOLT_CONFLICTS_RESOLVE`). DocuDolt's work is purely
presentation:
1. `doltConflicts` → call Dolt conflicts iterator → BSON response
2. `doltResolveConflict` → call Dolt resolve method → ok response
3. `doltMerge` already uses Dolt merge — it just needs to detect and surface
   conflict state rather than silently failing.

No new storage. No new conflict logic. Dolt owns it all.

### DoltHub Integration

If remote push/pull is implemented to be DoltHub-compatible (same chunk protocol
and ref format), DocuDolt databases become compatible with DoltHub — the largest public
database sharing platform. A DocuDolt user could `doltPush` to DoltHub and browse
their MongoDB data with the DoltHub web UI. This is a significant moat.

---

## 7. What "Version-Controlled MongoDB" Looks Like in Practice

A user-facing story for each tier of features:

**Basic (P1 features — near term)**:
> "I'm building a migration script. I want to test it on a branch, inspect the
> diff to verify my changes, then merge to main. If something goes wrong, I can
> reset to a known-good tag."
```javascript
db.getSiblingDB("mydb__d_migration").runCommand({doltBranch: 1, branch: "migration-v2"})
// ... run migration ...
db.getSiblingDB("mydb__d_main").runCommand({doltDiff: 1, from: "abc123"})
db.getSiblingDB("mydb__d_main").runCommand({doltMerge: 1, from: "migration-v2"})
db.runCommand({doltTag: 1, name: "pre-migration"})  // before the merge
```

**Intermediate (P2 features — mid term)**:
> "I want an audit trail. Show me every version of customer document _id=42, and
> which commit introduced the `gdpr_consent: true` field."
```javascript
db.runCommand({doltDocHistory: 1, collection: "customers", _id: 42})
db.runCommand({doltBlame: 1, collection: "customers"})  // who last touched each doc
```

**Advanced (P3 features — long term)**:
> "I have a dev DocuDolt instance and a prod DocuDolt instance. I push tested changes
> to prod. If prod goes wrong, I pull the last good state from DoltHub."
```javascript
db.runCommand({doltAddRemote: 1, name: "prod", url: "dolt://prod:27017/app"})
db.runCommand({doltPush: 1, remote: "prod", branch: "main"})
db.runCommand({doltPull: 1, remote: "backup", branch: "main"})
```

---

## 8. Conclusion

DocuDolt's version control foundation is solid. The Dolt storage layer already
provides the primitives for all features described above. The work is primarily
in exposing these capabilities through the MongoDB wire protocol.

The highest-leverage near-term features are:
1. **Point-in-time reads** (rootish `__d_` suffix) — one parsing change, zero new
   backend logic, immediate user value.
2. **List/delete branches + tags** — simple refsAM operations, complete the
   branch management workflow.
3. **Document history** — turns DocuDolt into an audit log database without any
   external tooling.

Together these three make DocuDolt genuinely useful as "version-controlled MongoDB"
for the most common use cases: branched development, audit trails, and snapshot
reads.
