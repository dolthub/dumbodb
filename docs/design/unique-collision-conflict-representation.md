# Conflict Representation for Index Collisions

**Issues:** related: workspace-k34 (build-time unique enforcement, distinct)
**Date:** 2026-06-23
**Status:** Implemented (workspace-pdz)

## 1. Goal

A merge can produce a conflict that is not about a single document
identity. The clearest case: a unique index, where each side of the
merge adds a *different* document (different `_id`) that lands on the
same indexed key. `dumboConflicts` should describe such a conflict
honestly -- what collided, on which index, and which documents are
contending -- using one conflict envelope that also still fits ordinary
document-edit conflicts.

Today it does not. The shape is primary-key-centric and a collision is
forced into it with a fabricated diff type that hides the actual cause.

## 2. What happens today

Setup (Scenario 4 in `docs/verify/index-merge.md`): unique index
`by_sku`; base holds `SEED`; `main` adds `{_id:10, sku:"S-1"}`,
`feature` adds `{_id:20, sku:"S-1"}`; merge `feature` into `main`.

dumbodb keeps `main`'s doc 10 ("ours wins"), evicts `feature`'s doc 20,
and parks doc 20 as a conflict. `doltConflicts` returns:

```js
{
  conflictId: "...",
  _id: 20,
  base: null,
  ours: null,
  theirs: { _id: 20, sku: "S-1" },
  ourDiffType: "deleted",      // hardcoded; merge_conflict.go
  theirDiffType: "added"
}
```

Problems:

- It is not a primary-key conflict. The two adds are on different
  `_id`s; at the primary-key level each is a clean add. The conflict is
  purely the secondary unique key.
- `ourDiffType: "deleted"` is incoherent: `base` and `ours` are both
  null, so nothing was deleted. `main` *added* doc 10.
- The cause is invisible. The surviving document (doc 10) and the
  colliding key (`sku:"S-1"`) appear nowhere in the conflict.

## 3. Why the current schema cannot express it

The envelope assumes one identity with three versions: a top-level
`_id`, and `base`/`ours`/`theirs` are versions *of that `_id`*. A unique
collision is two identities related by a shared key. There is no single
`_id`, so the loser's `_id` is borrowed and a diff type is invented.

## 4. Proposed envelope

One envelope for all conflict types, made self-describing by a `type`
discriminator and a `reason` (the explanation). The single structural
change: **`_id` moves off the top level and into each side.**

```js
{
  conflictId: "...",
  type: "documentEdit" | "uniqueKeyCollision",
  reason: {
    code:    "bothModified" | "modifyDelete" | "uniqueKeyCollision" | ...,
    message: "unique index \"by_sku\": ours and theirs both have sku = \"S-1\"",
    index:   "by_sku",        // present only when an index is implicated;
                              // always named in the message for collisions
    key:     { sku: "S-1" }   // the colliding key value
  },
  base:   { _id, doc } | null,
  ours:   { _id, doc, diffType } | null,
  theirs: { _id, doc, diffType } | null
}
```

- **documentEdit:** `ours._id == theirs._id == base._id` (the familiar
  single-identity case); `reason.index`/`reason.key` absent;
  `reason.code` distinguishes both-modified, modify/delete, etc.
- **uniqueKeyCollision:** `ours._id` and `theirs._id` may differ; each
  contending side carries `diffType: "added"`; `reason.index` and
  `reason.key` name what collided.

The `reason` field is the unifying element: every conflict, including an
ordinary document edit, carries a machine `code` plus a human `message`
explaining why the two states cannot both stand. A collision becomes
self-describing instead of requiring the reader to infer intent from
nulls.

Each side keeps its own `diffType` (`added` / `modified` / `deleted`) --
the mechanical fate of that document on that branch relative to base.
`reason.code` is orthogonal: it classifies why the sides are
*incompatible*. They answer different questions (what each branch did vs
why that clashes), so both are retained.

## 5. Why base/ours/theirs is sufficient (no N-way list)

A merge has at most two parents, so it is always a three-way merge:
base, ours, theirs. Each branch independently enforces its own unique
index, so no single branch can hold two documents with the same key.
Therefore, for a given (index, key) collision, each of base / ours /
theirs contributes **at most one** contending document. The triple is
not just adequate for collisions -- it is exactly right, once each slot
carries its own `_id`.

Two *independent* collisions are two conflicts, each with its own
`conflictId` and `reason.index`, resolved individually -- for example
one document pair colliding on `by_sku` and a separate pair colliding on
`by_code` (Scenario 4b). A *single* document pair that happens to
collide on two indexes at once is one conflict, not two: the loser is
evicted as a whole document on the first index that collides, so it can
no longer collide on the second. The conflict names the index that
triggered the eviction.

## 6. Resolution

The baseline is the standard conflict workflow and it already covers
collisions: the user updates their working state to remove the collision
(edit or remove one of the contending documents), then marks the
conflict resolved. No new verb required.

`ours`/`theirs` auto-resolution lands as a **separate commit** on top of
the representation change. For a collision it means "choose which `_id`
keeps the key; evict the other" -- not "pick a version of one `_id`".
The resolve command branches on `type`: `documentEdit` keeps its
existing meaning; `uniqueKeyCollision` interprets `ours` as "keep ours's
contender, evict theirs's" and `theirs` as the swap (evict ours's
winner, install theirs's).

This also fixes the current wart: resolving a collision with `theirs` is
rejected with a duplicate-key error (the model re-inserts the parked doc
while the winner still holds the key, instead of swapping key
ownership).

## 7. base semantics

- **add/add** (the real case): no document held the key in base ->
  `base: null`.
- **modify-onto-key / delete-then-readd**: best-effort, not
  load-bearing for v1. The triple still holds structurally (base's slot
  carries whichever `_id` held the key in the ancestor), but these are
  not prioritized.

## 8. Compatibility

Pre-1.0: changing the `dumboConflicts` wire shape (moving `_id` into the
sides, adding `type` and `reason`) is acceptable. The verify tests and
any consumers update with it.

## 9. Behaviors to pin (tests)

All DumboDB-only (no MongoDB analogue for merge):

- **Collision is self-describing.** Scenario 4 conflict carries
  `type: "uniqueKeyCollision"`, `reason.index: "by_sku"`,
  `reason.key: {sku:"S-1"}`, and both contending docs with their `_id`s
  -- not a null `ours` with `ourDiffType: "deleted"`.
- **Two indexes, two conflicts.** With two unique indexes and two
  independent colliding pairs (one pair on each index): exactly two
  conflict entries, one per index, each with its own `conflictId` and
  `reason.index`, resolvable independently (Scenario 4b).
- **Document edit still fits.** A same-`_id` divergent edit produces
  `type: "documentEdit"` with `base`/`ours`/`theirs` sharing the `_id`
  and a `reason.code` describing the edit clash.
- **Manual resolution clears it.** Removing the collision on the working
  branch and marking resolved completes the merge.

## 10. Related, out of scope

`workspace-k34`: `createIndex(unique)` does not reject *pre-existing*
duplicate values (build-time enforcement). Distinct from merge-time
collision representation; tracked separately.

## 11. Decisions

- **Per-side `diffType` is retained** (Section 4): each side records its
  branch's mechanical change to the document; `reason.code` separately
  classifies the clash.
- **`ours`/`theirs` collision auto-resolution is in scope**, as a
  separate commit on top of the representation change (Section 6).

## 12. Plan

1. **Representation (done):** the unified `dumboConflicts` envelope
   (`type`, `reason`, per-side `_id`/`doc`/`diffType`); capture both
   collision contenders at merge time; verify tests + `index-merge.md`
   updates.
2. **Resolution (done):** `ours`/`theirs` for `uniqueKeyCollision` (the
   key-ownership swap), replacing the dup-key rejection. `ours` keeps
   the surviving winner; `theirs` evicts it and installs the parked
   contender under the key.

The document-edit `reason.code` vocabulary settled as `bothModified`,
`modifyDelete`, and `deleteModify`.
