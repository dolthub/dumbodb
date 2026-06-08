# BSON Native Storage and IndexedBsonDocument

**Date:** 2026-06-03
**Status:** Design draft. Pending review before code lands.

---

## Motivation

The current write path serialises every document four ways on a round-trip:

```
wire (BSON in)
  -> types.Document          (BSON decode)
  -> wirebson.Document       (re-encode to BSON)
  -> BSON bytes
  -> Canonical Extended JSON (mongobson.MarshalExtJSON)
  -> JsonAdaptiveEnc tuple
```

and the read path inverts it:

```
JsonAdaptiveEnc tuple
  -> Canonical Extended JSON bytes
  -> mongobson.UnmarshalExtJSON
  -> BSON bytes
  -> types.Document          (BSON decode)
  -> wirebson re-encode
  -> wire (BSON out)
```

The CPU cost of these conversions is the focus -- not the on-disk
storage overhead, which is already acceptable. The Extended-JSON pass
in particular is a full reparse of the document on every write and
every read. ExtJSON encoding has no schema-shortcut: every numeric
gets a `{"$numberInt": ...}` wrapper, every ObjectId gets `{"$oid":
...}`, every date gets `{"$date": ...}`. Marshal walks the whole BSON
byte stream, unmarshal walks the whole JSON byte stream, and neither
direction can shortcut to "memcpy the typed bytes through."

Storage overhead of ~30 percent (synthetic workload) to 2-3x (real
Mongo-shaped workloads, traced to `_id ObjectId`, `createdAt Date`,
`updatedAt Date` blowing up under ExtJSON wrapping) remains a
secondary concern. Storage at 100k documents is already 3.6 MB after
the move to `JsonAdaptiveEnc`; the absolute number is fine.

## Direction

Store raw BSON in a `BytesAdaptiveEnc` value tuple, and implement an
`IndexedBsonDocument` analogous to dolt's `IndexedJsonDocument` for
out-of-band-spilled documents.

This removes the ExtJSON pass from both directions. Read becomes
`storage bytes -> types.Document`; write becomes `types.Document ->
storage bytes`. For documents that arrive over the wire as BSON,
several intermediate hops can be elided entirely once the encoder is
in place.

### Why not just keep ExtJSON

Three reasons:

1. **The conversion is CPU-pure.** It doesn't amortise across
   document size or batch size. Every document on every read pays it.
2. **The conversion isn't structural.** It buys no semantics dumbo
   uses internally -- types.Document is the source of truth for query
   evaluation, and it round-trips through BSON cleanly without needing
   the ExtJSON form ever.
3. **Dolt's JSON column features that ExtJSON enables (JSON path
   queries, `IndexedJsonDocument` mutation) all have BSON analogues
   that are simpler to build because BSON is a more regular format.**
   See [IndexedBsonDocument design](#indexedbsondocument-design).

### What we give up

- **SQL access to the doc column** via `SELECT doc FROM <collection>`
  through a MySQL driver returns opaque bytes. The current ExtJSON
  representation was preserved partly to demonstrate that data lives
  in dolt storage; that is not a long-term requirement. If a SQL-side
  decoder is wanted later, a `dumbo_decode(doc)` SQL function can
  emit ExtJSON on demand.
- **`extJSONFieldPatterns` filter prefilter** (`collection.go:243`).
  Replaced with a BSON-element prefilter; see
  [Filter pushdown](#filter-pushdown).
- **The `scanTopLevelNumericExtJSON` range walker**
  (`collection.go:533`). Replaced with a BSON top-level field walker,
  which is simpler because BSON encodes type and length explicitly.

We do **not** give up `IndexedJsonDocument`'s structural-sharing
property for OOB-spilled docs under partial update -- it transplants
to BSON.

### What we do not change

- The collection schema's _id key column (`binary(20)` of SHA-512
  hash) is unaffected.
- `AdaptiveValue` framing -- the same 1-byte inline header and 9-byte
  + 20-byte OOB encoding works for `BytesAdaptiveEnc` as it does for
  `JsonAdaptiveEnc`.
- Adaptive-encoding spillover threshold: still
  `DefaultTupleLengthTarget`.
- The mutation entry points (`applyFieldMutations`) keep their inline
  and out-of-band split. Implementations swap.
- Dolt: **no changes**. `BytesAdaptiveEnc` already exists and is
  exported. The collection's `doc` column becomes `varbinary` in the
  `TableSchema` flatbuffer; that is the only schema-side change.

## IndexedBsonDocument design

### What it is

A prolly tree of blob chunks whose leaves are byte segments of a BSON
document, plus AddressMap inner nodes keyed by a serialised path into
the document. This is the BSON analogue of dolt's `IndexedJsonDocument`
(`dolt/go/store/prolly/tree/json_indexed_document.go:40`).

Three observations make this transplant tractable:

1. **The chunking is byte-level, not JSON-level.** Dolt's blob nodes
   hold byte substrings. Reconstructing the document is byte
   concatenation. Nothing in the tree machinery cares that the bytes
   are JSON.

2. **The path encoding (`jsonLocation`) is content-agnostic.** It is
   a state byte plus a sequence of `(separator, key-or-index)` pairs.
   The 0xFF and 0xFE separators were chosen because they cannot
   appear in UTF-8; this property holds equally for BSON CString
   field names (which are UTF-8, NUL-terminated, no 0xFF byte).

3. **The scanner is a state machine that emits path boundaries.** The
   JSON scanner is one such state machine; a BSON scanner is another.
   BSON's is structurally simpler because:
   - Every element prefixes its type byte.
   - Containers (documents and arrays) prefix their total length.
   - Strings prefix their length; no escape-state machine.
   - No whitespace; no normalisation decisions.
   - No "is this a number or end-of-value?" ambiguity.

### Components

| File (proposed) | Mirrors (dolt) | Approx LOC | Notes |
|---|---|---|---|
| `bson_location.go` | `json_location.go` | ~500 | Path encoding. Could be lifted nearly verbatim. |
| `bson_scanner.go` | `json_scanner.go` | ~150 | Simpler than JSON. |
| `bson_chunker.go` | `json_chunker.go` | ~230 | Same `weibullCheck`, same level salts. |
| `bson_cursor.go` | `json_cursor.go` | ~250 | Direct port. |
| `bson_indexed_document.go` | `json_indexed_document.go` | ~700 | Lookup / Insert / Set / Remove. |

Roughly ~1800 LOC of new code, lifted in shape from dolt. Lives in
`dumbodb/internal/bsonindexed/` (new package). Uses dolt's exported
`tree` primitives (`NodeStore`, `Node`, `chunker`, `weibullCheck`,
`AddressMap`, blob serializer, level salts) -- none of these require
a dolt fork.

### Path encoding

`bsonLocation` is byte-identical in shape to `jsonLocation`:

```
state-byte || (separator || encoded-element)*
```

- state-byte: startOfValue / objectInitialElement / arrayInitialElement /
  endOfValue / middleOfValue
- object key separator: 0xFF, followed by UTF-8 field name bytes
- array index separator: 0xFE, followed by SQLite4 varint of the index

Lex byte order matches document traversal order. The `middleOfStringValue`
state from the JSON design is renamed `middleOfValue` -- in BSON it
represents being partway through a long string or binary blob value
within a leaf-spanning chunk.

Field ordering inside each path element is whatever the sort
requirement above produces -- lex order at every level.

### Document encoding requirement

Stored documents have object fields in lexicographic order at every
level -- top-level document and every nested sub-document. This is
required, not a default: deterministic sibling ordering is what makes
diff and merge well-defined. Clients send fields in arbitrary order,
so the encoder lex-sorts on write. Mongo clients do not rely on field
order being preserved across a round-trip, so this is observable but
harmless on read.

### Format choice (a) vs (b)

We will build both. The bake-off decides.

**(a) Raw BSON in leaves with ancestor length-prefix patching.**

Leaves contain genuine BSON byte substrings. Document and array
containers retain their inline 4-byte length prefixes. On mutation:

1. Locate the chunk containing the splice point via AddressMap walk.
2. Splice the new bytes into the leaf.
3. Re-chunk the affected region using the chunker.
4. Walk up the path and patch each ancestor container's length
   prefix. Each patch is a 4-byte little-endian write; BSON's
   16 MB doc limit ensures the prefix width never has to grow.
5. Each patched ancestor's chunk gets rewritten (its content hash
   changes), and the AddressMap entry pointing to it is replaced.

The ancestor length prefixes cluster near the start of each enclosing
container, so in typical Mongo docs (depth 1-3) the ancestor patches
often share a leaf chunk with the splice, collapsing D rewrites to 1
or 2. The bake-off measures this.

Read path is a streaming AddressMap walk: leaf bytes flow directly
to the wire as the cursor advances. Concatenation alone produces
wire-ready BSON because every container's length prefix is already
embedded in the leaf bytes. Zero buffering, zero re-encoding.

**(b) Envelope-less BSON in leaves; reconstruct on read.**

Leaves contain BSON elements concatenated without enclosing container
length prefixes. Each leaf is *not* a valid BSON document; it's a
fragment. Mutation:

1. Locate the chunk via AddressMap walk.
2. Splice the new bytes.
3. Re-chunk the affected region.
4. **No ancestor patching.** No length prefixes were stored.

Read walks the AddressMap, buffers container bodies in memory, and
emits `<length><body>` as each container closes. The outermost
container holds the whole document, so the full doc materialises in
memory before the first byte goes to the wire.

The cost reverses relative to (a): writes touch only the bytes of
the affected region; reads materialise the whole document on every
fetch.

Both (a) and (b) share `bson_location.go`, `bson_scanner.go`, and
`bson_chunker.go`. They diverge in how the chunker emits leaves and
how the cursor materialises bytes for read. The shared code is the
bulk; the diverging code is on the order of 200 LOC per branch.

### What is provably the same between (a) and (b)

- Storage size in chunks (modulo the 4-byte length prefixes themselves
  per container; negligible for shallow docs, measurable in deeply
  nested ones).
- Lookup-by-path cost (both walk the same AddressMap to find the
  containing chunk).
- Diff and merge semantics on top.

What varies is on the splice/read hot paths only.

## Filter pushdown

The current prefilter (`extJSONFieldPatterns`) builds a set of byte
substrings that must appear in a document's stored bytes if it matches
the filter, and runs `bytes.Contains` per stored doc. The substring
trick works because canonical Extended JSON gives every typed value a
unique byte representation.

A BSON prefilter is sounder, faster, and simpler. For a top-level
equality `{field: value}`:

1. The document's first 4 bytes are total length; the rest is a
   sequence of `<type><cstring-name>\x00<value>` elements with a
   trailing 0x00.
2. Walk the top-level elements only (no descent into nested
   documents). For each element, compare the cstring name against
   `field`. If it doesn't match, skip `<value>` using the type byte's
   known length (or the value's own length prefix for strings,
   documents, arrays, binary).
3. If the name matches, compare the value's bytes against the
   expected encoding of `value` for the type.

For range predicates against a top-level numeric field
(`scanTopLevelNumericExtJSON` today), the BSON walker reads the type
byte and decodes the value as int32 / int64 / double / decimal128
without prefix-matching strings or recovering from escapes.

This is concretely fewer lines of code than the current implementation
and harder to get subtly wrong.

## Open questions

### Format header

Reserve a 1-byte version header at the start of every stored document,
**at the top level only** -- sub-documents and embedded arrays do not
carry a version byte. The header sits before the BSON (or BSON
fragment) payload and is distinct from the `AdaptiveValue` 1-byte
inline marker, which lives in the tuple builder's framing one level
out.

Whichever format wins the bake-off ((a) or (b)) is version `0x01`.
There is no need to distinguish (a) and (b) at runtime: only the
winner ever lands on `main`, the loser branch is deleted, and the
version byte buys forward-compatibility for any *future* format
change after that.

Costs 1 byte per document.

## Testing approach

The testing plan has three layers: unit-level Go benchmarks, end-to-end
wire benchmarks, and instrumented storage tests. The same plan runs
against **three targets**:

- `main` -- current ExtJSON over `JsonAdaptiveEnc` (baseline).
- `bson-a` -- branch (a), raw BSON with ancestor length patching.
- `bson-b` -- branch (b), envelope-less BSON with read-time length
  reconstruction.

All three branches build against the same dolt SHA. Benchmarks run on
the same machine in the same shell. We use `benchstat` for paired
comparisons across branches.

### Fixture document shapes

The existing `dumbodb-parity-testing/benchmarks/` rig already defines
size buckets (`small ~100 B`, `1 KB`, `10 KB`, `100 KB`, `1 MB`). We
extend the fixture catalogue with three additional axes:

**Size axis** (already in place):
- `small` (~100 B): stays inline in tuple. Tests the inline write/read
  hot path without OOB involvement.
- `1 KB`: at the boundary; some docs inline, some OOB depending on
  `DefaultTupleLengthTarget`.
- `10 KB`: OOB but only a few leaf chunks.
- `100 KB`: OOB, deep enough chunk tree that AddressMap navigation
  cost matters.
- `1 MB`: pathological size; tests structural sharing under mutation.

**Type-richness axis** (new):
- `typed_minimal`: only `_id: ObjectId`, `a: int32` (today's synthetic
  doc).
- `typed_realistic`: `_id: ObjectId`, `createdAt: Date`,
  `updatedAt: Date`, `version: int32`, `userId: ObjectId`,
  `tags: [string]`, `metadata: Document`. This is the shape that
  blows up worst in ExtJSON (2-3x storage); it's where we expect
  branch (a) and (b) to most diverge from `main` in CPU cost.
- `typed_extreme`: ten ObjectIds and ten Dates at the top level. Stress
  test for prefilter substring scanning and for the ExtJSON wrapping
  tax.

**Depth axis** (new, for partial-update benchmarks only):
- depth-1: mutation at the top level.
- depth-3: mutation at `$.a.b.c`.
- depth-5: mutation at `$.a.b.c.d.e`.

**Mutation-kind axis** (new, for partial-update benchmarks only). The
existing rig only exercises `$set` on an existing scalar field, which
barely changes any container's length. To stress ancestor-length
propagation -- the core (a) vs (b) divergence -- we add mutation
kinds that grow or shrink containers:

- `array_extend`: `$push` on an array, adding one element. Grows the
  enclosing array's length prefix and every ancestor document's
  length prefix.
- `array_shorten`: `$pop` on an array, removing one element.
  Symmetric shrink case.
- `field_insert`: `$set` on a previously-absent field. Adds a new
  element to the enclosing document; grows that document and every
  ancestor.
- `field_remove`: `$unset` on an existing field. Removes an element;
  shrinks that document and every ancestor.

For (a), each of these requires patching D ancestor length prefixes
where D is the depth of the mutation. For (b), none of them touch
ancestors at all. This is the matrix where the format-choice cost
shows up most starkly.

**Inline vs OOB scope for partial-update tests.** Partial updates are
only interesting when the document is out-of-band. Inline documents
get a full rewrite of the AdaptiveValue payload on every mutation
regardless of mutation kind -- no chunk reuse is possible, no
ancestor length-prefix patching applies (the whole doc is one
contiguous inline blob), and (a)/(b) collapse to "are there length
prefixes embedded in this blob or not", which is a wash. Therefore
partial-update benchmarks run only at OOB sizes (10 KB, 100 KB,
1 MB). Inline sizes (small, 1 KB) are exercised by insert/read
benchmarks; they don't appear in the partial-update matrix.

### Layer 1: Unit-level Go benchmarks

Live in `dumbodb/internal/bsonindexed/` (the new package) and in
`dumbodb/internal/backends/dolt/` for the encode/decode wrappers.

Per branch:

```
BenchmarkEncode_<size>_<type>
BenchmarkDecode_<size>_<type>
BenchmarkPartialUpdate_<kind>_<size>_<depth>     # OOB sizes only
BenchmarkChunkerSplice_<size>_<depth>            # IndexedBsonDocument-internal
BenchmarkAddressMapWalk_<size>                   # IndexedBsonDocument-internal
```

Where `<kind>` is one of: `array_extend`, `array_shorten`,
`field_insert`, `field_remove`. The current `$set`-on-existing-field
benchmark is retained as a baseline but is not the focus -- it
exercises the chunker without exercising the ancestor-length
propagation that distinguishes (a) and (b).

Per benchmark we report:

- `ns/op`: wall-clock per operation.
- `B/op`: bytes allocated per operation.
- `allocs/op`: allocation count.

Encode and decode benchmarks **isolate the BSON / ExtJSON CPU cost**
from the storage path. They are pure in-process functions; no
tuple-builder, no node store, no chunker. Their job is to answer:
"how much CPU did we save by dropping ExtJSON?"

`BenchmarkPartialUpdate_*` exercises the full
`applyFieldMutationsInline` or `applyFieldMutationsOutOfBand` path
without going through the wire. This isolates the mutation cost from
SQL-side overhead and lets us see the (a) vs (b) split clearly.

`BenchmarkChunkerSplice_*` and `BenchmarkAddressMapWalk_*` are
internal benchmarks that exist only to localise regressions. They
won't appear in the bake-off summary table but are useful when one
of the higher-level benchmarks moves and we need to understand why.

### Layer 2: End-to-end wire benchmarks

Live in `dumbodb-parity-testing/benchmarks/`. Re-use the existing
size-bucketed benchmarks:

- `BenchmarkInsertOne_<size>` (`sized_bench_test.go:49`)
- `BenchmarkFindOne_<size>` (`sized_bench_test.go:67`)
- `BenchmarkUpdateOne_SetField_<size>` (`sized_bench_test.go:90`)

Add:

- `BenchmarkInsertOne_<size>_typed_realistic`
- `BenchmarkFindOne_<size>_typed_realistic`
- `BenchmarkUpdateOne_ArrayExtend_<size>_depth<d>`
- `BenchmarkUpdateOne_ArrayShorten_<size>_depth<d>`
- `BenchmarkUpdateOne_FieldInsert_<size>_depth<d>`
- `BenchmarkUpdateOne_FieldRemove_<size>_depth<d>`
- `BenchmarkAggregateFilter_<size>` -- runs a full-collection filter
  pushdown. Exercises the prefilter path: this is where the new BSON
  prefilter replaces `extJSONFieldPatterns`.

The four `UpdateOne_*` benchmarks above run only at OOB sizes
(10 KB, 100 KB, 1 MB) for the reason given in the mutation-kind axis.

Reporting: `benchstat` table per branch pair. We care about both
magnitude and direction. Wall-clock dominates here because the wire
round-trip is in the path.

**No commits during these benchmarks.** Layer 1 and Layer 2 measure
operation latency, not steady-state storage. Commits would add
chunk-store flushing and snapshot work to the timed path and obscure
the per-op comparison against MongoDB (which is the other reason
these benchmarks exist). The collection is dropped after each run.

### Layer 3: Storage and instrumentation

Live in `dumbodb-parity-testing/storage/`. We extend the existing
`TestStorageParity_DocSize` and `TestStorageParity_PartialUpdates`
tests to include `bson-a` and `bson-b` backends alongside `DoltJSON`
and `DumboDB`.

**Commit and GC discipline (required, not optional).** Storage
measurements are only meaningful when the chunk store has been
through a commit + GC cycle. Without commits, every intermediate
chunk produced by every mutation lingers in the working set, and the
on-disk byte count reflects garbage rather than steady state.
Structural sharing -- the entire point of (a) vs (b) on partial
updates -- only becomes observable across commits, because the
shared chunks are shared *between historical snapshots*. A single
huge mutation run with no commits would make (a) and (b) look
identical no matter what.

The existing `TestStorageParity_PartialUpdates` already does an
insert phase, commit, mutation phase, commit, then GC and measure.
For the mutation-kind tests we tighten this further:

- Insert phase: insert N docs, commit `base`.
- Mutation phase: apply M mutations, with a commit every `M/K`
  mutations (default K = 8 -- empirically gives chunk reuse a chance
  to show value without exploding test runtime).
- Final commit, then GC, then measure.

The commit cadence is a parameter; the bake-off may sweep it (K = 1,
4, 8, 32) on a subset of cells to see how (a) vs (b) storage costs
change with commit density. Real workloads commit at unpredictable
rates; we want to know the sensitivity.

Per backend, per fixture cell:

- Post-GC bytes on disk.
- Number of leaf chunks per document.
- Average leaf chunk size.

These come from existing instrumentation (`metrics.go`). They confirm
the storage shape and let us spot pathological cases (e.g. if (b)
unexpectedly produces tiny leaves because the chunker keys differ).

**New instrumentation for the bake-off:**

A per-mutation counter recording **leaves rewritten per splice**. This
is the key (a) vs (b) divergence: (a) rewrites ancestor leaves; (b)
does not. The counter is added behind a build tag and is consumed only
by the bake-off, not in production. Reported as histogram (p50, p99,
max) per partial-update benchmark cell.

### The bake-off matrix we actually run

Across all three branches:

| Benchmark | Size | Type | Depth | Kind | Why |
|---|---|---|---|---|---|
| Encode | small / 1KB / 10KB | minimal / realistic | -- | -- | Layer-1 CPU saving |
| Decode | small / 1KB / 10KB | minimal / realistic | -- | -- | Layer-1 CPU saving |
| InsertOne wire | small / 1KB / 10KB / 100KB | minimal / realistic | -- | -- | End-to-end write |
| FindOne wire | small / 1KB / 10KB / 100KB / 1MB | minimal / realistic | -- | -- | End-to-end read |
| UpdateOne ArrayExtend | 10KB / 100KB / 1MB | realistic | 1 / 3 / 5 | extend | (a) ancestor-patch cost |
| UpdateOne ArrayShorten | 10KB / 100KB / 1MB | realistic | 1 / 3 / 5 | shorten | (a) ancestor-patch cost |
| UpdateOne FieldInsert | 10KB / 100KB / 1MB | realistic | 1 / 3 / 5 | insert | (a) ancestor-patch cost |
| UpdateOne FieldRemove | 10KB / 100KB / 1MB | realistic | 1 / 3 / 5 | remove | (a) ancestor-patch cost |
| AggregateFilter | 10KB | realistic | -- | -- | Prefilter pushdown |
| Storage parity | small / 10KB | minimal / realistic / extreme | -- | -- | On-disk size |
| Leaves-rewritten | 10KB / 100KB / 1MB | realistic | 1 / 3 / 5 | all four | (a) vs (b) divergence |

The four UpdateOne mutation-kind rows are the most important cells
in the matrix. They are where (a)'s ancestor-patching cost is
expected to show up most clearly, and they are the only place where
the design choice between (a) and (b) has a meaningful answer.

Each cell runs `-benchtime=10s -count=10`. `benchstat` produces paired
diffs with confidence intervals.

### Decision criteria

The bake-off picks one of (a) or (b). The primary signal is the
four UpdateOne mutation-kind benchmarks (`array_extend`,
`array_shorten`, `field_insert`, `field_remove`) crossed with depth.
Reads are the secondary signal. Some patterns we expect, and how
we'll interpret them:

- **(a) reads faster, (b) writes faster on length-changing
  mutations** (expected baseline). (a) streams leaf bytes to the
  wire; (b) materialises each document in memory to add length
  prefixes. (b) trades that read-side cost for zero ancestor patching
  on mutation. Decision goes to whichever side's win is larger in
  absolute terms on the realistic-doc cells, given the assumption
  that reads outnumber writes.
- **(a)'s ancestor-patching cost is large in the deep-doc cells.**
  If the `leaves-rewritten` counter shows (a) rewriting many leaves
  per splice at depth 3 or 5 -- and the mutation-kind benchmarks
  reflect that in wall-clock -- pick (b). Repeated cheap mutations
  matter.
- **(a)'s ancestor-patching cost stays small because length prefixes
  cluster.** The expected best case: D ancestor prefixes share one
  or two leaves near the document start, so (a)'s leaves-rewritten
  count stays low even at depth 5. If this holds, (a) is preferred.
- **(b)'s materialisation cost grows pathologically at 1 MB.** If
  reads at the largest doc size show order-of-magnitude regressions
  from buffering the whole document, pick (a). Streaming wins when
  the doc is too large to want in memory.
- **Storage sizes diverge.** If one branch's chunker behaviour
  produces meaningfully smaller (or larger) chunks, dig in -- usually
  signals a tunable rather than a fundamental.
- **Both are within noise of each other.** Pick (a) for the property
  that leaf chunks are real BSON fragments (easier debugging).

The qualitative thresholds above are guesses written before any data
exists. The bake-off may surface a different pattern; if so, we'll
update the criteria with what we learn.

### What we are not testing

- **Concurrent write throughput.** The benchmarks are single-threaded.
  Multi-writer behaviour isn't expected to differ between (a) and (b);
  if it turns out to, that's a follow-up.
- **Long-running storage growth.** The parity-testing suite already
  covers anti-amortisation; (a) and (b) both inherit the
  `JsonAdaptiveEnc` inline-packing behaviour, so we don't expect
  drift here.
- **MySQL-side SELECT.** Explicitly out of scope; the doc column
  becomes opaque bytes from SQL's perspective.

### Variance control

- **Dedicated benchmark hardware.** The same physical box is used
  for every run, for consistency with previous benchmark runs against
  earlier dumbodb work. Each branch is checked out and benchmarked
  independently -- no side-by-side processes, no shared resources.
  The user drives this: check out `bson-a`, run the full suite, save
  results; check out `bson-b`, run the full suite, save results;
  compare offline.
- Same machine for all runs; no other heavy load.
- `GOGC=100`, default `GOMAXPROCS`.
- Each benchmark cell runs with `-benchtime=10s -count=10`, then
  `benchstat` compares.
- Seed data is generated from the same PRNG seed (`bench.seed=42`,
  existing convention) so docs are byte-identical between runs.
- Storage tests run on a fresh data directory each time; the harness
  already does this.

## Workplan

The intent is that this design doc lands first, gets reviewed, and
only then do we start cutting code. Each step below would be a bd
issue. Roughly in order:

1. **This design doc** (current step). Land in `dumbodb/docs/design/`.
2. **Benchmark plan implementation.** Extend
   `dumbodb-parity-testing/benchmarks/` with the typed-realistic /
   typed-extreme fixtures, the four mutation-kind operations
   (`array_extend`, `array_shorten`, `field_insert`, `field_remove`),
   the depth axis, and the commit-cadence sweep on the storage side.
   This is real code; lives on `main` of parity-testing so both
   branches benchmark against the same harness.
3. **Branch `bson-a`.** Format version `0x01`. Raw BSON in leaves
   with ancestor length-prefix patching. New package
   `dumbodb/internal/bsonindexed/`. Switches the collection schema
   and write/read paths in `dumbodb/internal/backends/dolt/`.
4. **Branch `bson-b`.** Format version `0x01`. Envelope-less BSON
   in leaves with read-time materialisation. Same package layout as
   `bson-a`.
5. **Bake-off run.** Each branch is benchmarked independently on
   dedicated hardware. Results compared offline; pick a winner.
6. **Land the winner on `main`.** Delete the loser branch.

### Branching strategy

Both branches are implemented **as if each is the winner**. No
prototype-quality shortcuts, no "we'll productionise it after the
bake-off." After the bake-off we ship one of them; we do not want a
rewrite phase between "this won" and "this is in production."
Concretely:

- Both branches use format header byte `0x01`. There is no need to
  distinguish them at runtime -- only the winner exists after the
  bake-off.
- The branches are **independent**. They do not share a working
  branch and do not get merged together. The user checks out one,
  benchmarks it, then checks out the other.
- **Shared code** (scanner, chunker, path-location encoding,
  cursor, address-map walking) either lives in initial commits at
  the base of both branches, or is duplicated outright. Either is
  fine. Duplication is cheaper when there's any risk of the shared
  code drifting between branches under the pressure of
  format-specific needs; shared-base commits are cheaper when the
  code really is identical.
- Test coverage on each branch matches what we would require for
  landing on `main`: unit tests for the scanner, chunker, and path
  encoding; integration tests for the write/read paths; storage
  parity tests passing.

Steps 3 and 4 are independent; they can be done in parallel once
step 2 lands. The benchmark harness on `main` of parity-testing is
the only thing shared.

## Status

Design draft for review. No code changes yet.
