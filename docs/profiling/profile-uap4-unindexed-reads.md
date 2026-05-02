# Profile: unindexed read benchmarks (do-uap4)

PROFILE ONLY. No fix. The bead asked: where does the 4–10x slowdown vs MongoDB
come from on these three parity benchmarks?

| Parity bench               | DumboDB    | MongoDB | Ratio |
| -------------------------- | ---------- | ------- | ----- |
| `Find_FilterEq_50K`        | 143.39 ms  | 23.92 ms | 5.0x |
| `Agg_Sort` (sort+limit 1K) |   6.89 ms  |  1.26 ms | 4.5x |
| `Agg_MatchGroup` (1K)      |   6.41 ms  |  0.68 ms | 8.4x |

## Method

Mirrored each parity bench in-process against the dolt backend in
`internal/backends/dolt/profile_bench_test.go`, chaining the same iterators
the wire-level handler builds (`common.FilterIterator`, aggregation
`stages.NewStage(...).Process`). This keeps the dominant code paths but lets
`-cpuprofile` see them under their real symbols, without going through the
wire bridge or per-bench DB lifecycle.

Run with:

```
go test -run='^$' -bench=BenchmarkProfile_<name> -benchtime=NX \
  -cpuprofile=/tmp/X.cpu -memprofile=/tmp/X.mem ./internal/backends/dolt/
go tool pprof -top -cum -focus=BenchmarkProfile_<name> /tmp/X.cpu
```

Per-iter wall times measured here (-benchtime=30x for find, 200x for agg):
- `BenchmarkProfile_FindFilterEq_50K`: 199 ms/op
- `BenchmarkProfile_AggSort`: 17.9 ms/op
- `BenchmarkProfile_AggMatchGroup`: 18.2 ms/op

(In-process numbers are higher than parity wire numbers because there's no
prepared-cursor / first-batch cutoff and `b.N` re-runs the full scan every
iteration. The shape of the profile is what matters for this bead.)

## Single dominant pattern

**All three benchmarks bottleneck on the same code path: per-document
ExtJSON-to-types.Document decode in `mapIter.Next` during the full-scan
read.** Below, percentages are cumulative CPU time within the bench focus
filter (loop, post-`b.ResetTimer`).

### Find_FilterEq_50K (50K docs, ~5K match grp=k)

| % cum | Function | Notes |
| ----- | -------- | ----- |
| 49.5% | `dolt.(*mapIter).Next`           | sequential prolly-map walk + decode |
| 21.9% | `dolt.readDocJSONBytes`          | chunk-store fetch + JSON serialize  |
| 18.2% | `dolt.decodeDocFromJSON`         | ExtJSON → BSON → types.Document     |
| 15.0% | `bson.UnmarshalExtJSON` (driver) | ExtJSON → BSON Raw                  |
| 21.2% | `runtime.mallocgc` (flat 3.0%)   | GC/alloc churn                      |

Read-side prefilter (`buildScanPrefilter`) is active here  -- it skips full
decode for misses  -- but with grp equality at 1/10 selectivity, ~5K of 50K
docs survive the prefilter and pay full decode cost.

### Agg_Sort (1K docs, $sort{i:-1} + $limit:100)

| % cum | Function | Notes |
| ----- | -------- | ----- |
| 84.0% | `stages.(*sort).Process` / `common.SortIterator` | drains+sorts |
| 75.9% | `dolt.(*mapIter).Next`           | upstream scan/decode dominates |
| 74.8% | `dolt.readDocJSON`               | full read+decode (no prefilter) |
| 66.7% | `dolt.decodeDocFromJSON`         | as above |
| 55.8% | `bson.UnmarshalExtJSON` (driver) | as above |
| 29.6% | `runtime.mallocgc`               | GC/alloc churn |

Sort algorithm itself accounts for ~9% of the stage's time
(`SortIterator` 84% − upstream `ConsumeValues` 76% = ~8%). The slurp+decode
of all 1000 input docs dominates; the sort and the $limit:100 truncation
are negligible.

### Agg_MatchGroup (1K docs, $match{grp:{$gte:0}} + $group{_id:$grp,...})

| % cum | Function | Notes |
| ----- | -------- | ----- |
| 81.7% | `stages.(*group).Process` / `groupDocuments` | drains+groups |
| 80.4% | `common.(*filterIterator).Next`  | $match implemented as FilterIterator |
| 76.6% | `dolt.(*mapIter).Next`           | upstream scan/decode |
| 74.8% | `dolt.readDocJSON`               | full read+decode |
| 65.9% | `dolt.decodeDocFromJSON`         | ExtJSON → BSON → Document |
| 52.8% | `bson.UnmarshalExtJSON` (driver) | as above |
| 31.2% | `runtime.mallocgc`               | GC/alloc churn |

The $match has an operator-doc value (`{$gte:0}`), so the
`buildScanPrefilter` path bails out  -- every doc is fully decoded. The
`{$gte:0}` predicate matches every doc, so $match passes 1000/1000 docs to
$group. Group's hash bucketing is ~1% of stage time
(`group.Process` 82% − upstream filterIterator 80% = ~1.5%).

## Common pattern

Every benchmark looks like the same picture rotated:

```
unindexed scan ─┬─ readDocJSONBytes  ──── prolly chunk fetch + json marshal
                │                            │
                └─ decodeDocFromJSON ──── mongobson.UnmarshalExtJSON ──┐
                                            │                          │
                                            └─ extJSONParser.advanceState
                                                                       │
                                            decodeDocument ←───────────┘
                                            (BSON → types.Document)
```

The downstream stage (filter / sort / group) is consistently <10% of
the stage's CPU time. The expensive step is **ExtJSON → BSON → Document
double-conversion**, applied to every doc that survives any byte prefilter.

Two compounding costs:

1. **JSON encoding round-trip.** Docs are stored as JSON in dolt prolly tree
   (`tree.JSONDoc.ToIndexedJSONDocument` + `sqltypes.MarshallJson`), then
   converted JSON → ExtJSON-aware BSON (`mongobson.UnmarshalExtJSON`), then
   BSON → `types.Document` (`bson.ToDocument`). Two parse phases per doc.
   `bson.UnmarshalExtJSON` alone is 53–56% of the agg benches.

2. **GC/alloc churn.** `runtime.mallocgc` consumes 21–31% of CPU across all
   three. Top flat hotspots are `mallocgcSmallScanNoHeader`,
   `nextFreeFast`, `mallocgcSmallNoscan`, `writeHeapBitsSmall`. The
   ExtJSON parser and the document builders allocate per-field; nothing in
   the read path reuses buffers across documents.

## Where the gap to MongoDB lives (inferred, not measured)

MongoDB stores docs natively as BSON; it doesn't transit ExtJSON. So the
`UnmarshalExtJSON` half of `decodeDocFromJSON` (≈half the per-doc cost)
has no analogue. That alone plausibly accounts for a 2x gap. Combine with
allocation overhead and the lack of a per-iteration buffer reuse strategy
and the observed 4–10x is consistent with a decode-bound pipeline against a
storage-native one.

## Files added on this branch

- `internal/backends/dolt/profile_bench_test.go`  -- three
  `BenchmarkProfile_*` functions wired through the same handler iterators
  that the wire path uses, suitable for `-cpuprofile`.
- `docs/design/profile-uap4-unindexed-reads.md`  -- this report.

No production code touched.
