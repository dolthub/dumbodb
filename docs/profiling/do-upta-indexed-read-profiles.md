# do-upta  -- Profiling indexed read benchmarks

Bead: do-upta. Profile-only  -- no fixes, just findings.

## Summary

All five operations correctly route through their backend index fast paths.
The performance gap to MongoDB has **two distinct shapes**:

1. **Find-style ops (`find_filter_eq`, `agg_match_group`)**  -- the index path
   runs but each matched document is fetched and **decoded from canonical
   Extended JSON to BSON to `types.Document`** on every Get. With ~5K matches
   per call at 50K, ExtJSON parsing alone burns ~35% of CPU. This is the
   storage-format penalty: MongoDB ships native BSON to the wire, DumboDB
   round-trips through ExtJSON.

2. **Count-style ops (`count_documents_*`)**  -- the backend `Count` finishes in
   53 us (10K) / 207 us (50K). Parity reports 17 ms / 98 ms. The 99% gap is
   **above the backend, not inside it**  -- handler / aggregate-shortcut /
   wire-marshal overhead. Backend Count already hits `tryIndexedCount` ->
   `RangeCount` (single index walk, no doc fetches).

Distinct scans every index entry (50K) for what is structurally a 10-group
walk; the prolly cursor walk dominates and there is no seek-skip to the next
distinct prefix.

## Method

Backend-level CPU/mem profiles via `go test -cpuprofile -memprofile` against
new benchmarks added in `internal/backends/dolt/profile_bench_test.go`.
Profiling at the backend layer attributes time to DumboDB code without wire
or driver framing, and the per-op timings track the parity numbers closely
for the find-shaped ops, validating that the backend dominates them.

```bash
go test -run=^$ -bench=BenchmarkProfile_FindFilterEq_50K_Indexed \
  -benchtime=100x -cpuprofile=cpu.out -memprofile=mem.out \
  ./internal/backends/dolt
go tool pprof -top -cum cpu.out
```

## Per-test findings

### find_filter_eq_50k_indexed  -- 105 ms/op (parity: 81 ms vs Mongo 14.78 ms)

Index path is used. `tryIndexLookup` runs the index-range walk, capped at 50%
of the collection (5K matches out of 50K -> admitted), and point-fetches each
primary id.

Top-3 cumulative CPU under `BenchmarkProfile_FindFilterEq_50K_Indexed`:

| Rank | Function | Cum % | Role |
|------|----------|-------|------|
| 1 | `dolt.readDocJSON` | **48.19%** | Per-id primary doc fetch + decode |
| 2 | `bson.UnmarshalExtJSON` | **34.06%** | ExtJSON -> raw BSON |
| 3 | `bson.copyDocument` / `copyDocumentCore` | **27.45%** | BSON re-encoding inside the unmarshal path |

Inside `readDocJSON` the split is 88% decode, 12% chunk-store fetch:

```
readDocJSON                       11.87s  (cum)
  readDocJSONBytes                 1.45s   (chunk read + JSON marshal)
  decodeDocFromJSON               10.42s
    bson.UnmarshalExtJSON          8.40s   (ExtJSON parser)
    decodeDocument                 1.94s   (BSON -> types.Document)
```

**Pattern: 80% of decode time is the ExtJSON parser.** The collection stores
canonical Extended JSON in the prolly tree's value side; every doc read pays
the parse cost. MongoDB's native BSON storage avoids this entirely  -- a read
is closer to a memcpy.

`runtime.mallocgc` reaches 24% from the same path (every parsed doc allocates
new BSON structures).

### count_documents_10k  -- 53 us/op backend (parity: 17 ms vs Mongo 1.94 ms)

Index path is used. `Count` -> `tryIndexedCount` -> `idx.RangeCount` walks the
secondary-index range and counts entries without primary fetches.

Top backend functions inside `Count`:

| Rank | Function | Cum % (backend) | Role |
|------|----------|-----------------|------|
| 1 | `index.RangeCount` | dominant | Index range walk |
| 2 | `prolly/tree.OrderedTreeIter.Next` | inside RangeCount | Per-entry advance |
| 3 | `(*collection).tryIndexedCount` | thin wrapper | Index/bounds setup |

**The backend is not the bottleneck  -- it is 327x faster than the parity
number reports.** With backend Count at 53 us and parity at 17 ms, ~99% of
end-to-end time lives above the backend: in `MsgAggregate` (the v2 driver's
`CountDocuments` sends an aggregate pipeline) or `MsgCount`, in the
shortcut-pattern match, in OpMsg framing, or in the cursor reply assembly.
The aggregate-count shortcut wired in commit `2f6969d` (do-37r9) does the
right thing once it fires; the open question is whether it fires for the
exact pipeline shape the v2 driver emits in this benchmark.

### count_documents_50k  -- 207 us/op backend (parity: 97.83 ms vs Mongo 7.93 ms)

Same shape as the 10K case. RangeCount scales linearly with the matched
range (5K entries here vs 1K), 207 us / 5K ~= 41 ns per index step  -- that is
near the prolly cursor's structural lower bound. No hot-spot to address at
this layer. Same handler/wire gap conclusion as the 10K case.

### agg_match_group_indexed  -- 91 ms/op backend (parity: 6.53 ms vs Mongo 0.65 ms)

Bench shape: `[{$match: {grp: x}}, {$group: {_id: "$grp", c: {$sum: 1}}}]`.
The benchmark's per-iteration cost equals find_filter_eq's because the
backend work is identical  -- the $match feeds Query, then the $group reads
every match and accumulates. The group accumulator itself is invisible in
the profile.

> **Note on parity number.** The bead lists `agg_match_group_indexed: 6.53 ms`,
> which is much faster than `find_filter_eq_50k_indexed: 81 ms` despite being
> the same underlying scan. This suggests the parity benchmark for
> `agg_match_group_indexed` measures a smaller collection (likely 10K, not
> 50K), or a much narrower match. The backend-level cost shape is identical
> to find-eq; same fix-points apply.

Top-3 cumulative CPU (same as find-eq, identical hot path):

| Rank | Function | Cum % |
|------|----------|-------|
| 1 | `dolt.readDocJSON` | 45.07% |
| 2 | `bson.UnmarshalExtJSON` | 32.06% |
| 3 | `bson.copyDocument*` | 25.69% |

### distinct_50k_indexed  -- 4.2 ms/op backend (parity: 6.41 ms vs Mongo 0.60 ms)

`DistinctScan` index-path is used. The backend already avoids the per-row
primary fetch  -- only one primary lookup per *unique* value (10 in this
seed).

But the index walk **reads every one of the 50K index entries** to find
those 10 unique prefixes. Top inline cost in `scanDistinctFromIndex`:

```
scanDistinctFromIndex                   840 ms (cum)
  iter.Next                             470 ms   (56%)   -- prolly cursor advance
  idxKeyDesc.GetBytes                   180 ms   (21%)   -- extract composite key bytes
  bytes.Equal(prefix, prevPrefix)       110 ms   (13%)   -- same-group dedup
  prevPrefix = append(...)               40 ms   ( 5%)
  out = append(out, val)                 30 ms   ( 4%)
  lookupFieldFromPrimary                 10 ms   ( 1%)
```

**Pattern: per-entry scan over an inherently sparse-result space.** With
50K index entries / 10 distinct values, MongoDB's DISTINCT_SCAN seeks past
the current value's range to the next prefix instead of reading 5K entries
per group. Implementing that here turns 50K cursor advances into 10 seeks
plus 10 primary lookups.

`lookupFieldFromPrimary` is already cheap (1% of distinct, 10 fetches) so
the per-doc decode cost that hurts find-eq is irrelevant here.

## Common patterns across tests

1. **All five operations correctly hit their index fast path**  -- no fallback
   to collection scan, no missing index, no 50%-range gate kicking in.
   The plumbing is right.

2. **Storage format penalty: ExtJSON <-> BSON.** Every read of a primary
   document goes through `readDocJSON -> decodeDocFromJSON ->
   bson.UnmarshalExtJSON`, which dominates `find_filter_eq_50k` (~35% of
   total CPU) and `agg_match_group_indexed` (~32%). MongoDB's native BSON
   storage skips this entirely. This is the highest-leverage fix-point for
   selective-but-not-tiny matches.

3. **Per-id primary fetch loop.** When the index path matches K rows, the
   backend issues K independent `prolly.Map.Get` lookups against the
   primary tree. Even after eliminating ExtJSON decode, each Get is a
   prolly tree traversal. A batched primary-fetch (sort ids, single
   ordered cursor walk over the primary tree) would let cache locality do
   the work the per-id loop fights against.

4. **Count and Distinct already avoid primary fetches.** `RangeCount` and
   the distinct index walk only touch the index. So the find-eq fix-points
   above don't apply to them.

5. **`count_documents_*` is bottlenecked above the backend.** Backend
   Count is 53 us / 207 us but parity reports 17 ms / 98 ms. ~99% of the
   reported wall-time is in the handler / aggregate-shortcut / wire path.
   The follow-up question: does `tryCountAggregateShortcut` actually fire
   for the exact pipeline the Go v2 driver emits? An end-to-end trace
   (logging at the shortcut entry point) would answer this in one run.

6. **`distinct` walks every index entry**, not just one per distinct
   value. With high duplication ratios this is asymptotically wasteful;
   the prolly cursor's `Seek` API would let the scan jump from group to
   group.

## Suggested follow-up beads (not implementing here)

- **find / agg_match_group**: store BSON natively in the prolly tree value
  (or cache decoded `types.Document` per chunk) to eliminate the ExtJSON
  parse on every Get. Largest single win.
- **find / agg_match_group**: batch primary fetches when the index returns
  ids in sorted order  -- single tree walk instead of K Gets.
- **count_documents**: end-to-end trace of `MsgAggregate` to verify the
  count-aggregate-shortcut matches the v2 driver's CountDocuments pipeline,
  or whether the shortcut declines and the path falls through to a full
  $group accumulator.
- **distinct**: switch `scanDistinctFromIndex` from `IterAll` to a
  seek-based loop that advances past each unique prefix's range.

## Reproducing

The new file `internal/backends/dolt/profile_bench_test.go` adds five
profiling benchmarks. Each is independently runnable. Sample command and
the exact pprof commands used for this report:

```bash
mkdir -p /tmp/profile-do-upta

go test -run=^$ -bench=BenchmarkProfile_FindFilterEq_50K_Indexed \
  -benchtime=100x \
  -cpuprofile=/tmp/profile-do-upta/find_eq_50k_cpu.out \
  -memprofile=/tmp/profile-do-upta/find_eq_50k_mem.out \
  ./internal/backends/dolt

go tool pprof -top -cum -nodecount=30 /tmp/profile-do-upta/find_eq_50k_cpu.out
go tool pprof -list readDocJSON       /tmp/profile-do-upta/find_eq_50k_cpu.out
go tool pprof -list decodeDocFromJSON /tmp/profile-do-upta/find_eq_50k_cpu.out
```

Repeat for `BenchmarkProfile_CountDocuments_10K_Indexed` (`-benchtime=500x`),
`BenchmarkProfile_CountDocuments_50K_Indexed` (`-benchtime=200x`),
`BenchmarkProfile_AggMatchGroup_50K_Indexed` (`-benchtime=100x`),
`BenchmarkProfile_Distinct_50K_Indexed` (`-benchtime=200x`).
