# TTL Indexes in a Version-Controlled Database

**Issues:** (ticket TBD) -- driven by parse-server startup, which creates a TTL
index on `_Idempotency.expire`; DumboDB currently rejects TTL with
`InvalidOptions (72)`.
**Date:** 2026-08-07
**Status:** Discussion / Draft -- **design only, no code changes yet**

## 1. Goal and scope

Decide whether and how DumboDB supports MongoDB TTL indexes given that DumboDB is
a version-controlled (Dolt-backed) store. Naively, TTL is antithetical to version
control: MongoDB's TTL is a background thread that *silently* deletes documents on
a wall clock, which fights the core promise that a commit's content is fixed and
reproducible.

The reframe that makes it tractable: **in a version-controlled DB, an expiry is an
event, not a disappearance.** MongoDB throws away the fact that a record expired
and when; DumboDB can preserve it as a commit -- auditable and revertible. That
turns TTL from "antithetical to VC" into a feature that *showcases* VC.

This doc leads with verified MongoDB background (section 2), states the core
tension and the decisions taken so far (sections 3-4), and collects the open
questions (section 5). It is a discussion document; nothing here is implemented.

## 2. Background: how MongoDB TTL works (verified against 8.0.4)

A TTL index is an ordinary single-field index on a **date** field, plus an
`expireAfterSeconds` option. A background "TTL monitor" thread runs **every 60
seconds** and deletes documents whose expiration time has passed.

Verified facts (probed against MongoDB 8.0.4):

- **Uniform expiration:** `expireAfterSeconds: N` (N > 0). A document expires when
  `indexedDate + N <= now`. All documents under the index share the same lifetime
  relative to their date field.
- **Per-document expiration:** `expireAfterSeconds: 0`. The document's date field
  value **is** its expiration instant -- it expires when `indexedDate <= now`. This
  is how you give each document its own TTL: store the desired expire-at time in
  the field. (Answers "can you set the TTL on a single document" -- yes, via this
  mode.)
- **Changing a document's TTL:** update the indexed date field on the document.
  The next sweep re-evaluates against the new value, so updating the field
  reschedules (or cancels, by moving it far into the future) that document's
  expiry.
- **Changing the index-wide TTL:** `collMod {index: {name|keyPattern,
  expireAfterSeconds: M}}` changes the lifetime for *all* documents under the
  index. Verified: `100 -> 500` took effect and is reflected in `listIndexes`.
- **Single-field only:** a compound TTL index is rejected --
  `CannotCreateIndex: "TTL indexes are single-field indexes, compound indexes do
  not support TTL."`
- **Non-date / missing / non-conforming field:** documents whose indexed field is
  missing, not a date, or an array without a date are **not** deleted. For an
  array of dates, MongoDB uses the **minimum** date.
- **Timing is not exact:** deletion happens within ~60s after expiry (monitor
  period) plus delete time. Applications must not assume prompt deletion.
- **`_id` cannot be a TTL index.** Partial TTL indexes (`partialFilterExpression`
  + `expireAfterSeconds`) are allowed. TTL is moot on capped collections (DumboDB
  has none).
- `expireAfterSeconds` range is `0 .. 2147483647` (int32 seconds).

Observable contract we are matching: *documents whose expiration has passed stop
being returned and are removed, within roughly a monitor period.*

## 3. Design principle: expiry is a commit on the live tip

The one choice to fix up front: a TTL sweep is a normal delete **commit that
advances a branch's live tip** -- never a read-time filter that hides
"currently-expired" documents.

Given that, reproducible history is **not** a concern (an earlier draft wrongly
billed it as a tension):

- A revision-qualified read -- `getSiblingDB("db@<commit>")`, or any tag/hash/`^`
  form -- is an **immutable snapshot**. TTL sweeps advance tips; they never rewrite
  history. A query against a pinned revision therefore returns the same result
  forever, regardless of server TTL activity. It is literally a point in time.
- A branch's **live tip** does move over time -- but it always has, on every write.
  A TTL sweep is just another writer; two reads of a *live* tip straddling a sweep
  differ no more than two reads straddling a user's `deleteMany`.

So there is no tension between TTL and version control. The only reason to state
this explicitly is to rule out the tempting shortcut of read-time expiry
filtering, which would be non-deterministic and is simply unnecessary once expiry
is a commit.

Corollary: a document is "expired" precisely when a **sweep commit** has removed
it from the tip -- not the instant its timestamp passes. Between sweeps an
expired-but-unswept document is still present, which matches MongoDB's own
"within ~a monitor period" looseness.

## 4. Design decisions taken so far

These were decided in discussion on 2026-08-07:

1. **Sweep every 60 seconds**, matching MongoDB's monitor cadence and observable
   timing. Hard-coded for now; a configurable interval is deliberately deferred to
   a future dial.
2. **One pass evaluates all branches at the same instant**, using a single shared
   `now` cutoff. Rationale: **merge-friendliness.** A document shared between two
   branches (common history, unchanged) has the same date field on both, so a
   single-instant sweep deletes it on both branches at once. Merging the branches
   afterward therefore does not re-animate it. (A branch that independently updated
   the field to a later time simply is not expired there yet -- correct, that is
   different data.)
3. **One pass evaluates all collections and all TTL indexes at once** -- the sweep
   is global, not per-collection.
4. **One commit per branch per sweep.** The drops for a branch -- every expired doc
   across all its collections/indexes -- are batched into a single commit on that
   branch. Not one commit per document, not one per collection. A literal single
   commit spanning all branches is not possible (a commit is one snapshot on one
   ref); "all branches at once" is the shared-`now` invariant, realized as N
   commits (one per affected branch) produced in one pass.

Corollaries / implications to work through:

- A sweep that finds nothing on a branch makes **no commit** there (no empty
  no-op commits every 60s).
- Expiry commits are **revertible** (`dumboRevert`) -- a capability MongoDB cannot
  offer. They should carry a distinct author/message (e.g. author `ttl`, message
  summarizing counts per collection) so they are legible in `dumboLog`.

## 5. Transactionality and session isolation

Priority: **get per-branch behavior right, with each branch swept in isolation and
in parallel.** The sweep is modeled as an internal writer that opens a **session /
transaction per branch**, reusing DumboDB's existing `dsess`-based commit path
(fork-point snapshot + three-way merge on commit + conflict surfacing) -- the same
machinery user writes and `dumboCommit` use. This gives per-branch isolation and
parallelism for free: each branch is an independent `dsess.branchState`.

The shared `now` (section 4.2) and per-branch transactions compose cleanly: the
cutoff is captured once at the start of the cycle and used everywhere, but each
branch's snapshot -> delete -> commit is its own transaction.

Requirements on the per-branch transaction:

1. **Isolation & parallelism.** Each branch's sweep is an independent transaction;
   sweeping branch A never touches branch B's state, so branches sweep in parallel.
2. **No lost updates.** The sweep commits by three-way-merging its deletes against
   the branch's *current* tip (which a user session may have advanced since the
   sweep's snapshot), never by blind overwrite. `dsess.doCommit` already does this.
3. **The sweep yields to concurrent user edits (key correctness rule).** A
   TTL-delete is *advisory*: it applies only to a document unchanged since the
   sweep's snapshot. If a user concurrently modified the doc -- e.g. rescheduled it
   by moving its date field to the future, or deleted it -- the sweep's delete for
   that doc is **dropped**, not forced. delete/delete converges to deleted;
   modify-vs-TTL-delete resolves to the user's modify (the doc is no longer
   expired). A background sweep must **never surface a merge conflict to users**.
4. **Per-branch atomicity.** The batch is one commit -- all-or-nothing for that
   branch. If the commit loses a race or would conflict, that branch's sweep for
   this cycle is **abandoned and retried next cycle** (60s later); no partial state.
5. **Never wedged.** The sweeper must not block user writes or get stuck behind a
   paused merge / dirty working set on a branch; if a branch is not in a
   cleanly-committable state, skip it this cycle.

Deferred alternative (noted, not chosen): a **single root-level update that bumps
all branches forward atomically** in one transaction. Dolt's chunk store has one
root mapping branch -> commit, so this is conceivable and would make the whole
sweep atomic across branches. But it couples all branches into one transaction --
one branch's conflict would stall the others -- which is at odds with the
per-branch isolation we are prioritizing. Revisit only if cross-branch atomicity
turns out to matter.

## 6. Open questions

1. **Which branches are swept?** All local branches with a TTL index and something
   expired, presumably. Tags / detached commits are read-only history and are
   excluded by section 3. A branch not in a cleanly-committable state is skipped
   this cycle (section 5, req 5) -- is "skip and retry next cycle" acceptable, or is
   a more active resolution wanted?
2. **`expireAfterSeconds: 0` (per-document / scheduled expiry)** -- support in full,
   or subset out initially? parse-server does not need it, but it is the more
   powerful mode (the date field *is* the per-doc expiry instant).
3. **`collMod` to change `expireAfterSeconds`** and **updating the date field to
   reschedule** -- both must be honored by the next sweep. Storage must keep the
   index's `expireAfterSeconds` mutable (ties into the index-metadata store,
   workspace-alp.16 / bvb).
4. **Author identity / permissions** for the `ttl` sweep commits, and whether they
   appear in diff/status/log exactly like user commits.
5. **Staging vs. big-bang.** Minimum to unblock parse-server = accept the TTL index
   + *some* eventual reclamation. The full auditable-event design above is larger.
   Decide whether to ship a minimal accept-and-sweep first.

## 7. Non-goals (current thinking)

- Read-time expiry filtering (rejected -- non-deterministic and unnecessary once
  expiry is a commit; see section 3).
- Sub-monitor-period precision.
- TTL on capped collections (DumboDB has none) or `_id`.
