# Document Storage Parity with Dolt's JSON Column

**Issue:** workspace-2c9
**Date:** 2026-06-02

---

## The gap

The `dumbodb-parity-testing` storage suite (`storage/storage_scale_test.go`)
inserts N canonical `{_id, email, name, age}` documents into three backends,
commits once, runs GC, and walks the data directory. Post-deindex (no
secondary indexes on any side):

| N    | DoltTyped | DoltJSON | DumboDB | Dumbo/DoltJSON |
|------|-----------|----------|---------|----------------|
| 10k  | 264 KB    | 302 KB   | 1.2 MB  | 3.99x          |
| 100k | 2.6 MB    | 2.3 MB   | 21.2 MB | **9.20x**      |

Dumbo's per-document on-disk cost grows with N (120 -> 212 bytes/doc going
10k -> 100k); DoltJSON's shrinks (30 -> 23). Anti-amortisation.

## What dolt does

A `JSON` column on a Dolt table is encoded as `val.JsonAdaptiveEnc`. The
write path lives at `go/store/prolly/tree/prolly_fields.go:509`:

```go
case val.JsonAdaptiveEnc:
    j, err := convJson(ctx, v)
    buf, err := types.MarshallJson(ctx, j)
    if err = tb.PutAdaptiveJsonFromInline(ctx, i, buf); err != nil { ... }
```

The JSON bytes are written **inline into the row tuple**. Many rows then
pack into a single prolly-map leaf chunk, and snappy compresses across
the packed leaf. Small documents pay no per-document chunk overhead.
Larger documents spill out-of-band as raw-byte blobs (see
`JsonAdaptiveEncodingScriptTests` at
`go/libraries/doltcore/sqle/enginetest/dolt_queries.go:6231`).

The JSON chunker (`SerializeJsonToAddr`) is **not** invoked on inline
writes -- instrumentation at 10k confirmed zero `SJTA` events from the
DoltJSON path.

## What dumbo does today

`writeDocJSON` (`internal/backends/dolt/collection.go:2716`) routes
every document through the chunker:

```
types.Document
  -> BSON bytes (wirebson.Encode)
  -> Canonical Extended JSON (mongobson.MarshalExtJSON, canonical=true)
  -> sqltypes.NewLazyJSONDocument(extJSONBytes)
  -> tree.SerializeJsonToAddr -> one prolly-tree root per document
```

Each document produces one leaf chunk in the JSON chunker (10k SJTA
events / 10k LEAF events for a 10k-doc workload, ~96 bytes Extended
JSON per doc). The collection's prolly map stores `(_id_hash -> JSONAddr
hash)` tuples; the document content itself lives in its own chunk.
Result: NBS chunk overhead is paid once per document and there is no
cross-document compression at the leaf-chunk level.

## Path to sync

1. **Change the collection map's value descriptor** so the document
   column is `val.JsonAdaptiveEnc` instead of `val.JSONAddrEnc`. This
   is in dumbo's per-collection `valDesc` construction (see
   `collection.go` and the schema helpers under `internal/backends/dolt/`).
2. **Replace `writeDocJSON`** with the JsonAdaptiveEnc write idiom:
   produce the JSON bytes (still going via canonical Extended JSON for
   BSON-type fidelity, unless we adopt a different on-disk encoding)
   and call `tb.PutAdaptiveJsonFromInline` instead of building a
   `LazyJSONDocument` and calling `SerializeJsonToAddr`.
3. **Read path:** `valDesc.GetJSONAddr` no longer returns a hash that
   indirects to a chunker root; use the adaptive accessor
   (`GetJsonAdaptiveValue`) to fetch the inline bytes (or
   `JsonAdaptiveStorage` if spilled out-of-band).
4. **Mutation path:** `applyFieldMutations` currently relies on
   `IndexedJsonDocument.Set` / `.Remove` for structural sharing on
   partial updates. The adaptive path will need an equivalent --
   Dolt must already have one for `UPDATE ... SET j = JSON_SET(...)`
   on adaptive-JSON columns. Finding and instrumenting that mechanism
   is tracked under `bd workspace-110` and should be done after the
   insert-side fix lands.
5. **Storage upgrades:** existing dbs hold `JSONAddrEnc` values. Decide
   whether to migrate on read, on write, or with an explicit step.

The out-of-band threshold for `JsonAdaptiveEnc` is set by the tuple
builder; documents above it spill to raw-byte blobs, which is what
DoltJSON already does for large values. The fast path we want is the
inline path for typical Mongo documents.

## How to verify

The `storage-parity-instr` branches in `dolt` and `dumbodb` carry the
instrumentation used to find this. To verify a fix:

1. Cherry-pick `storage-parity-instr` onto the fix branch in both
   repos. The dumbodb commit re-adds the
   `replace github.com/dolthub/dolt/go => /workspace/dolt/go`
   directive; adjust the path if the dolt checkout lives elsewhere.
2. Build dolt and dumbodb, restart the parity-testing servers.
3. Truncate `/tmp/dolt-instr.log` and `/tmp/dumbo-instr.log`, then run
   `STORAGE_PARITY_MAX_DOCS=10000 go test ./storage -run TestStorageParity_Scale -v`.
4. Expected post-fix signal: `SJTA` events from the dumbo path drop to
   zero (inline writes do not go through the chunker), `WRITE_DOC`
   events disappear, and DumboDB / DoltJSON ratio drops toward 1.0x
   at 10k and 100k. Tighten the budgets in
   `storage/storage_scale_test.go:maxDumboOverDoltJSON` as the ratio
   shrinks.
