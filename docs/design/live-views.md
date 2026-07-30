# Live (Computed) Views

**Issues:** workspace-z0i (epic: live views). Related: workspace-5fg
(materialized views, separate), workspace-alp.16 (collection-metadata
persistence), docs/design/collation-resolution.md (view collation).
**Date:** 2026-07-28
**Status:** Draft

## 1. Goal and scope

Define how DumboDB should implement MongoDB **standard (computed) views** so it
matches MongoDB 8.0, and close the gap between that and what exists today (basic
in-session read-through, but non-durable and incomplete).

In scope: view creation, durable storage, read resolution (including nesting),
the full set of read commands, redefine/rename/drop, name collisions, view
collation, and how views behave in a versioned (Dolt) store.

Out of scope: **materialized views** -- these are not a distinct object; they
are the `$merge`/`$out`-into-a-collection pattern, owned by workspace-5fg. The
comparator/ICU questions (collation-resolution.md) are referenced, not
re-opened.

Reference platform is MongoDB 8.0.4. Facts labelled "verified" were probed
directly; everything else is marked (verify) and pinned by the test matrix.

## 2. What a live view is (reference-verified)

A view is `{name, viewOn, pipeline, collation}`. It stores no data. A read
against a view resolves to: run `pipeline` over the source `viewOn`, then apply
the caller's own filter/sort/skip/limit/projection on top -- i.e. a read on a
view is an aggregation over its source. Writes are rejected.

Verified against MongoDB 8.0.4:

- **Nested views**: a view may be defined on another view; reads resolve through
  the chain (v2 on v1 on base returned the correctly double-filtered row).
- **Cycles**: detected and rejected ("View cycle detected").
- **Max nesting depth = 20**: the 20th level errors with
  `ViewDepthLimitExceeded` ("View depth too deep or view cycle detected").
- **Redefine**: `collMod {viewOn, pipeline}` re-defines a view in place (a
  redefine that narrowed the pipeline changed the read from 2 docs to 1).
- **distinct**: works on a view.
- **Name collision**: creating a view with the name of an existing collection ->
  `NamespaceExists`.
- **Writes**: insert/update/delete on a view -> `CommandNotSupportedOnView`.
- **listCollections**: reports `type:"view"` and `options:{viewOn, pipeline,
  collation}` (collation fully resolved).

## 3. Current DumboDB state (audit)

Model: `{name, viewOn, pipeline}` held in an in-memory map `state.views`; a read
resolves via `viewSourceIterator` -> `buildViewPipelineStages` over the source.

Works today (in a running server; parity-tested Full):

- create (`create {viewOn, pipeline}` / driver `createView`), read-only;
- read via **find, count, aggregate** (pipeline applied, caller semantics layered);
- `listCollections` reports `type:"view"` + options;
- writes rejected with `CommandNotSupportedOnView`;
- drop a view.

Gaps (code-confirmed; see anchors):

| # | Gap | Evidence |
|---|-----|----------|
| G1 | RESOLVED (workspace-z0i.1) -- view def is now a durable `BlobFileID` entry in the collections AddressMap (`view_storage.go`); `state.views` removed | was: `database.go:256` set `state.views` then returned before `updateAddressMap` |
| G2 | RESOLVED (workspace-z0i.3) -- `distinct` resolves views via `viewSourceIterator`; `listIndexes`/`collStats` on a view return CommandNotSupportedOnView | was: `msg_distinct.go` had no `IsView` branch |
| G3 | RESOLVED (workspace-z0i.2) -- read path resolves the view source chain (`resolveViewChain`, view_pipeline.go) with cycle + depth-20 checks; create validates acyclicity | was: source fetched as a base collection, no resolution |
| G4 | RESOLVED (workspace-z0i.4) -- `collMod {viewOn,pipeline}` rewrites the view's metadata blob in place; cyclic/too-deep redefinitions are rejected | was: `viewOn` in collMod's ignored-fields list |
| G5 | RESOLVED (workspace-z0i.5) -- rename of a view is rejected with CommandNotSupportedOnView ("cannot rename view: <ns>"), matching MongoDB, which does not support renaming a view | was: rename silently lost the view |
| G6 | **`$lookup` in a view loads the whole foreign collection** into memory; foreign-is-a-view unresolved | `view_pipeline.go:57` fetcher `ConsumeValues` |

## 4. Design

### 4.1 Where metadata lives in the data tree (the keystone)

This is the root of G1/G5: DumboDB has no durable home for a view definition.
Establish the current tree, then place the view in it.

**Current tree (verified).**

- A database branch persists as a Dolt RootValue (RTVL); its `TablesBytes()` is
  the collections **AddressMap**: `name -> chunkHash` (`amFromWorkingRoot`).
- A collection's chunk is a `serial.Table` (DTBL): `Schema` (a single shared hash
  today, `backend.go:1036`), `PrimaryIndex` (the document prolly map),
  `SecondaryIndexes` (a prolly AddressMap, `indexName -> index-entry chunk`),
  plus conflicts/violations/artifacts (`buildDoltTableFlatbuffer`).
- **Secondary indexes are the only per-collection metadata we persist**, and
  they are the mechanism worth copying: each index is a content-addressed chunk
  (`index_persist.go`, `docToBSON` + `ns.WriteBytes`) referenced from the tree,
  so it is committed/branched/merged/diffed for free.
- Collections are always DTBL now -- the legacy bare-prolly-node ("TUPM")
  collection form was removed (pre-1.0, no back-compat). `serial.Table` has no
  free-form metadata slot and the `Schema` slot is shared.

**Chunks are type-tagged.** Every chunk carries a 4-byte FileID
(`serial.GetFileID`). After the TUPM removal the collection dispatch has one live
case: `TableFileID -> collection`.

**Views are blob chunks.** A view has no documents, so it needs no `serial.Table`.
Store a view as an AddressMap entry whose value is a **`BlobFileID` chunk** -- the
type `ns.WriteBytes` already produces, and exactly how index-entry chunks are
stored -- holding `docToBSON` of a self-describing metadata document:

```
{
  type:      "view",              // discriminator (see below)
  viewOn:    <string>,            // source namespace (collection or another view)
  pipeline:  [ <stageDoc>, ... ],
  collation: { locale, strength, ... } | null
}
```

Namespace classification is then a two-step, no-flag check:

```
chunk = store.get(collectionsAM.get(name))
switch serial.GetFileID(chunk):
  case TableFileID:  -> collection
  case BlobFileID:   def = bsonToDoc(ns.ReadBytes(hash))
                      switch def.type:
                        case "view": -> view
                        // future kinds ...
  default:           -> error
```

**Why the `type` discriminator.** FileID says "this is a metadata blob, not a
table"; the `type` field says *which kind*. Only `"view"` exists today, but
making the blob self-describing lets the same blob-in-AddressMap mechanism carry
other standalone namespace metadata later -- add a `type` value and a case, with
no new FileID and no format change.

**Properties.** The blob is referenced from the collections AddressMap, which is
part of the committed RTVL working set, so a view is durable, branch-scoped, and
committed/branched/merged/diffed with no special path. create/drop/rename reduce
to AddressMap add/remove/move on the name -- fixing G1 and G5 uniformly. No fake
empty table, no schema-comment flag. (`CreateCollection`'s current view branch,
`database.go:255-263`, which sets in-memory `state.views` and returns before
`updateAddressMap`, is replaced by writing the blob entry.)

**Data-bearing namespaces (collections).** A view is standalone. A collection
keeps its `serial.Table` for data, so its metadata (default collation, and later
validators) must attach to that collection. Materialized views are NOT a case
here: for parity they are ordinary collections with no stored definition
(workspace-5fg).

Size is not a constraint. Metadata is a BSON document stored as a blob via the
same plumbing as documents and index-entry chunks (`docToBSON` + `ns.WriteBytes`),
which spills large values into an out-of-band blob tree -- a large `$jsonSchema`
validator is just a large blob. So the only question is *where a collection
references its metadata blob from*, collision-free:

**Decided: a reserved entry in the collection's `SecondaryIndexes` AddressMap.**
The metadata is a BSON blob stored under the reserved key
`__dumbo_metadata__` (`backends.ReservedMetadataIndexName`) in the same map that
holds index-entry chunks -- the exact index-entry plumbing (`docToBSON` +
`ns.WriteBytes`), no indirection, any size. The blob is self-describing
(`{type:"collMeta", collation, validator, ...}`) so future metadata rides along.
Collision with a real index closes by reserving the name: `createIndex` rejects
it (implemented, alongside the `_id_` reservation) and `listIndexes` filters it
out. Shared with workspace-alp.16 (collection default collation).

Audited the alternatives (O6): the only unused `serial.Table` field that is a
usable reference is `Violations`, but it is a deprecated/legacy conflict slot
(Dolt folded conflicts+violations into `Artifacts`, which DumboDB uses) and
name-misleading; `AutoIncrementValue` is a scalar; the spare `TableSchema` fields
(`Checks`, `TargetRowSize`, comment) live in the *shared* schema and are
SQL-semantic. A namespace-descriptor blob (`{type, data:<tableHash>,
meta:<blobHash>}`) stays the clean-but-indirect fallback if we ever want a fully
uniform namespace model. The reserved entry wins: live mechanism, existing
plumbing, one small reject-rule.

### 4.2 Read resolution, nesting, cycles, depth

Resolve `viewOn` recursively: if the source is itself a view, prepend the inner
view's resolved pipeline, repeating until a base collection is reached. Enforce:

- **cycle detection** -> error matching MongoDB (`View cycle detected`, code to
  confirm) (verify O2);
- **max depth 20** -> `ViewDepthLimitExceeded` at the 20th level.

The resolved pipeline then runs over the base collection; the caller's
find/count/aggregate semantics layer on top as today.

### 4.3 Read-command coverage

Every read path must resolve views, not just find/count/aggregate. DONE
(workspace-z0i.3): **distinct** resolves the view source (G2); **listIndexes**
and **collStats** on a view return CommandNotSupportedOnView, matching MongoDB
(a view has no indexes or backing storage). `getMore` needs no view-specific
handling -- it iterates a cursor already opened by a view-resolving find or
aggregate. Each has a parity matrix row.

### 4.4 Redefine (collMod)

DONE (workspace-z0i.4): `collMod {viewOn, pipeline}` rewrites the view's metadata
blob in the catalog and re-validates the new source chain (cycle/depth). View
default collation via collMod is deferred to the collation epic. Toggling a
collection <-> view is not supported (view fields on a real collection are
ignored, as before).

### 4.5 Rename, drop, collisions

- rename: DONE (workspace-z0i.5). MongoDB does not support renaming a view;
  DumboDB rejects it with CommandNotSupportedOnView ("cannot rename view: <ns>").
- drop: remove the catalog entry (done via the AddressMap in workspace-z0i.1).
- create over an existing name (collection or view) -> `NamespaceExists`
  (workspace-z0i.5: a no-options create over an existing view is no longer
  idempotent). V10 (create view over existing collection) now reports the
  existing collection's UUID exactly as MongoDB does -- DumboDB emits the same
  message and the test asserts they are identical once the UUID value is
  normalized out. V6/V11 assert error code + codeName parity rather than
  byte-identical messages, since MongoDB's text spells out a namespace chain that
  need not be reproduced.

### 4.6 View collation

A view carries a default collation that is the effective collation for reads
against it that do not specify their own (see collation-resolution.md 2.1,
dimension I). The catalog (4.1) stores it; resolution treats the view as the
"target" default. This is why live-view durability and collation share the
catalog.

### 4.7 Version-control semantics (DumboDB-specific)

Because view defs live in the branch working set (4.1, blob entries in the
collections AddressMap):

- creating/dropping/redefining a view is an uncommitted working-set change until
  `dumboCommit`, and must surface in `dumboStatus`/`dumboDiff`.
- **Views are first-class in the diff (DONE, workspace-z0i.7).** Views are
  top-level namespaces -- siblings of collections, not per-collection sub-objects
  like indexes. `dumboDiff`, `dumboStatus`, and `dumboLog` all emit a single
  unified `changes` array, one entry per changed namespace, each tagged with a
  `type` discriminator (`collection` or `view`) and a `status`
  (`added`/`modified`/`deleted`). Verbosity is one shape at two fill levels, not
  two keys: a summary entry carries counts, a full entry carries documents.
  - A **collection** entry groups its detail under `documents` (`added`/
    `removed`/`modified`), `indexes` (`added`/`removed`/`modified`), and
    `metadata` (reserved, empty for now). At summary verbosity the sub-objects
    hold counts/names; at full verbosity they hold the documents/diffs.
  - A **view** entry carries `from`/`to` view definitions (`{viewOn, pipeline}`),
    `from` null for an added view and `to` null for a removed one (a redefine
    carries both).
  - `dumboDiff` returns full-verbosity `changes`. `dumboStatus` returns summary
    `changes`. `dumboLog` attaches `changes` per commit: `stat:1` fills summary
    verbosity, `patch:1` fills full verbosity (`patch` supersedes `stat` when
    both are set).
- **Implementation note:** the diff walks the collections AddressMap; each
  changed entry must be classified by chunk type (a `serial.Table` -> collection
  diff of docs+indexes; a view blob -> a view add/remove/modify) so a view is
  never mis-diffed as a collection's documents. Same classification the read path
  uses (4.1).
- branching a DB carries its views; merging two branches merges the AddressMap,
  so a view added on one side merges in.
- reading `mydb@<oldcommit>` sees the views as of that commit.
- branch + merge DONE (workspace-z0i.6): a view lives in the branched
  AddressMap, so branching carries it and a view added or dropped on one side
  merges in cleanly. A view redefined divergently on both branches is a merge
  conflict that pauses the merge and is resolved interactively via
  doltConflicts (reports the view conflict with base/ours/theirs definitions and
  a "view:<name>" conflictId) and doltResolveConflict (theirs/ours/custom, where
  custom supplies a {viewOn, pipeline} definition), then dumboMerge continue
  commits. View conflicts persist in the merge-state file (inline BSON) so an
  in-progress view conflict survives a restart. Verified by view_versioning_test.go.

**View-definition merge conflicts (DumboDB-only; no MongoDB oracle).** A view
redefined (or created, or dropped) divergently on both branches is a conflict on
that AddressMap entry. This must surface and resolve through the same conflict
workflow indexes and documents use (`doltConflicts` to inspect, `doltResolveConflict`
with `theirs`/`ours`/`custom` to resolve; see docs/verify/index-merge.md scenarios
4-6). Requirements:

- `doltMerge` stops with an unresolved-conflict error when a view entry diverges
  (redefine/redefine, redefine/drop, create/create-with-different-def under the
  same name).
- `doltConflicts` reports a self-describing view conflict naming the view and
  carrying `ours`/`theirs`/`base` view definitions (`{viewOn, pipeline,
  collation}`), so the operator can choose.
- `doltResolveConflict` accepts `theirs`, `ours`, and `custom` (a supplied view
  definition), and the resolved definition is what reads against the view see
  after the merge commits.
- create/create and redefine/drop variants each produce a conflict, not a silent
  last-writer-wins.

This is a version-control feature with no MongoDB counterpart, so it is **not**
parity-tested. DONE (workspace-z0i.9): specified and verified by the human
verification document `docs/verify/view-merge.md` and its matching automated
test `tests/verify/view_merge_test.go` (`TestViewMergeVerify`, per
docs/verify/README.md), covering clean merge, divergent-redefine with
theirs/ours/custom resolution, and the redefine/drop conflict.

The status/diff *shape* mirrors MongoDB's absence of a versioning analogue by
being ours to define; the *content* (view defs) matches what MongoDB's
`listCollections` reports. O3.

## 5. Parity test matrix (reference MongoDB 8.0)

Each row is a `harness.PairTest`. Support: Full where we must match, XFail where
we knowingly diverge today, MongoOnly if out of scope.

| ID | Case | Expected (verified 8.0.4 unless noted) | Support |
|----|------|----------------------------------------|---------|
| V1 | create view; find/count/aggregate apply pipeline + caller filter | correct rows | Full (have) |
| V2 | insert/update/delete on view | CommandNotSupportedOnView | Full (have) |
| V3 | listCollections | type:"view", options{viewOn,pipeline,collation} | Full (have) |
| V4 | distinct on a view | works (matches base+pipeline) | Full (workspace-z0i.3) |
| V5 | nested view (v2 on v1 on base) | resolves through chain | Full (workspace-z0i.2) |
| V6 | view cycle | error (GraphContainsCycle) | Full (asserts code+codeName; message not compared -- MongoDB spells out the namespace chain) |
| V7 | nesting depth 20 | ViewDepthLimitExceeded at level 20 | Full (workspace-z0i.2) |
| V8 | collMod {viewOn,pipeline} redefine | new definition applied to reads | Full (workspace-z0i.4) |
| V9 | rename a view | CommandNotSupportedOnView ("cannot rename view") | Full (workspace-z0i.5) |
| V10 | create view named as existing collection | NamespaceExists | Full (DumboDB reports the existing collection's UUID like MongoDB; messages match once the UUID value is normalized out) |
| V11 | create collection named as existing view | NamespaceExists | Full (asserts code+codeName; message not compared -- MongoDB spells out "is a view on <viewOn>") |
| V12 | durability: create view, restart servers (same data dir), read | view still resolves | Full (catalog persists views; workspace-z0i.1) |
| V13 | view with $lookup pipeline | correct join | Full (have) |
| V14 | view default collation applied to a no-collation read | collation semantics (dimension I) | XFail |

### 5.1 Version control -- dumbodb repo only (no Mongo oracle)

Version-control behavior is verified in the **dumbodb repo** (Go / bats), not
the parity harness, because `dumbo*` commands have no MongoDB counterpart:
a view is branch-scoped (invisible on `main` until merged);
`dumboStatus`/`dumboDiff` surface it as
a `type: "view"` entry in the unified `changes` array (4.7); branch/merge of
view defs.

**Merge-conflict resolution** for divergent view definitions (4.7) is verified by
a dedicated human verification document `docs/verify/view-merge.md` plus its
automated equivalent `tests/verify/view_merge_test.go` (README.md), covering the
redefine/redefine, redefine/drop, and create/create conflicts and the
`theirs`/`ours`/`custom` resolutions.

### 5.2 Durability across restart -- parity `PairTest` via a new harness primitive

Persisting a view across restart is a parity behavior (MongoDB keeps it; DumboDB
loses it today, G1), so it is a `PairTest` (V12). It needs a new harness
capability, because today neither server model can restart on the same data dir:
the shared servers spawn once (`provisionOnce`), and `EphemeralServers.Stop`
kills the processes AND removes the data dirs (`serverProc.stop` ->
`os.RemoveAll(dir)`).

**New primitive (dumbodb-parity-testing/harness): a private, restartable server
pair.** A non-auth sibling of `EphemeralServers` with a `Restart(t)` method that:

1. shuts each server down **gracefully** (SIGTERM / the `shutdown` command, not
   SIGKILL) so the test exercises durable state, not crash recovery;
2. **keeps** the data dirs -- a kill-without-`RemoveAll` variant of
   `serverProc`; final teardown (`t.Cleanup`) still removes them;
3. relaunches each binary on its **same data dir**, on a fresh free port;
4. waits for readiness and returns reconnected clients / new URIs.

The durability test drives its own private pair (not the shared-server
collection model, since a restart would disrupt other tests) -- exactly how the
auth tests use `EphemeralServers`.

**Test shape (V12):** start the restartable pair, create a view on both servers,
`Restart(t)`, reconnect, assert the view still resolves on both. MongoDB passes;
DumboDB is XFail until the catalog (4.1) lands, then Full.

The same primitive generalizes to any future durability parity test (collection
metadata, validators, collection default collation).

## 6. Open questions

- O1: View storage is decided (4.1): a `BlobFileID` AddressMap entry holding a
  self-describing `{type:"view", ...}` BSON doc. Remaining open items: the
  per-COLLECTION metadata carrier (Schema `Comment` for small vs SecondaryIndexes
  map with a reserved, collision-proof key for large validators) -- shared with
  workspace-alp.16; and confirming `listCollections`'s new per-entry chunk read
  is acceptable (it currently reads no values).
- O2: Exact MongoDB error codes for cycle and depth (message captured; codes to
  confirm) and for collMod on views.
- O3: Version-control semantics for view defs. Diff granularity DECIDED (4.7):
  a view is a `type: "view"` entry in the unified `changes` array carrying
  `{name, status, from, to}`, a sibling of collection entries (not the three
  per-collection index arrays).
  Merge-conflict resolution DONE (4.7, workspace-z0i.6): divergent view defs
  conflict on the AddressMap entry and resolve via
  `doltConflicts`/`doltResolveConflict` (`theirs`/`ours`/`custom`), like
  index/doc conflicts; DumboDB-only. No MongoDB reference.
- O4: `$lookup`/`$graphLookup` inside a view -- keep the load-whole-foreign
  approach (G6) or stream; and resolve a view-typed foreign target.
- O5: Interaction with capped/timeseries/validator metadata -- one catalog for
  all of it, or separate stores.
- O7: RESOLVED -- (A). Restart-durability is a parity `PairTest` (V12) via a new
  restartable-pair harness primitive (5.2), not a dumbodb-repo test. The
  primitive is a prerequisite for V12.
- O6: RESOLVED (2026-07-29). Per-collection metadata is a self-describing BSON
  blob stored under the reserved key `__dumbo_metadata__` in the collection's
  SecondaryIndexes AddressMap; `createIndex` rejects that name (implemented),
  `listIndexes` filters it. Namespace descriptor blob remains the documented
  fallback. Materialized views do not drive this (parity MVs are plain
  collections). Shared with workspace-alp.16.

## 7. Phasing

1. **Parity matrix (section 5)** first: land V1-V14 as Full/XFail against the
   oracle. This includes building the 5.2 restartable-pair harness primitive so
   V12 (durability) is a real `PairTest`. Version-control tests (5.1) live in the
   dumbodb repo.
2. **Catalog** (4.1): durable, versioned per-branch storage of view defs; flips
   V12 to Full. Coordinate with workspace-alp.16. DONE (workspace-z0i.1) for
   create/drop/list/read durability: a view is a `BlobFileID` blob in the
   collections AddressMap, classified on read by chunk type; `state.views`
   removed. Per-collection metadata carrier (`__dumbo_metadata__`) still pending.
3. **Nesting + cycles + depth** (4.2): flips V5-V7. DONE (workspace-z0i.2):
   `resolveViewChain` flattens the source chain on the read path (find/count/
   aggregate) with cycle (GraphContainsCycle) and depth-20 (ViewDepthLimitExceeded)
   checks; create-time validation rejects cyclic/too-deep view definitions. V6
   stays XFail only on MongoDB's exact cycle-message text.
4. **Command coverage** (4.3): distinct etc.; flips V4.
5. **Redefine + rename + collisions** (4.4/4.5): flips V8-V11.
6. **View collation** (4.6) once the catalog exists; flips V14.
7. **Version-control semantics** (4.7): branch scoping + dumboDiff/dumboStatus
   surfacing; verified by the 5.1 dumbodb-repo tests. dumboDiff/dumboStatus
   surfacing DONE (workspace-z0i.7): dumboDiff/dumboStatus/dumboLog emit a
   unified `changes` array with a `type` discriminator; a view is a
   `type: "view"` entry ({name, status, from, to}) and counts a view change as
   dirty. Merge semantics + conflict resolution remain (workspace-z0i.6).
</content>
