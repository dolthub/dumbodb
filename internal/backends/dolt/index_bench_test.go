// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dolt

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Mirrors newTestBackend for benchmarks, which take *testing.B not *testing.T.
func newBenchBackend(b *testing.B) *Backend {
	b.Helper()
	dir, err := os.MkdirTemp("", "dolt-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })

	bk := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	b.Cleanup(bk.Close)
	return bk
}

// Seeds a collection with the parity harness shape (docs with i, grp, tag).
// The seed runs in benchmark setup so it is not counted in the per-iteration
// timer.
func seedBenchCollection(b *testing.B, n int) (backends.Collection, context.Context) {
	b.Helper()
	ctx := context.Background()

	backend := newBenchBackend(b)

	db, err := backend.Database("benchdb")
	if err != nil {
		b.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("c")
	if err != nil {
		b.Fatalf("Collection: %v", err)
	}

	// Batched to avoid one giant InsertAll dominating the seed phase.
	const batch = 1000
	for off := 0; off < n; off += batch {
		size := batch
		if off+size > n {
			size = n - off
		}
		docs := make([]*types.Document, 0, size)
		for j := 0; j < size; j++ {
			i := int32(off + j)
			docs = append(docs, must.NotFail(types.NewDocument(
				"_id", i,
				"i", i,
				"grp", i%10,
				"tag", "row",
			)))
		}
		if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs, SkipDurableSync: true}); err != nil {
			b.Fatalf("InsertAll: %v", err)
		}
	}
	return coll, ctx
}

// Mirrors what the handler does after Query; its cost dominates the indexed
// path, so the benchmark must include it.
func drainIter(b *testing.B, it types.DocumentsIterator) int {
	count := 0
	for {
		_, _, err := it.Next()
		if err != nil {
			if err == iterator.ErrIteratorDone {
				break
			}
			b.Fatalf("Iter.Next: %v", err)
		}
		count++
	}
	it.Close()
	return count
}

// Mirrors parity's BenchmarkFind_FilterEq_10K_Indexed: equality on a 10-way
// bucketed field returns ~1K of 10K.
func BenchmarkIndexLookup_Equality_10K(b *testing.B) {
	const n = 10_000

	b.Run("unindexed_scan", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("grp", int32(i%10))),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})

	b.Run("indexed", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{
				{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			},
		}); err != nil {
			b.Fatalf("CreateIndexes: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("grp", int32(i%10))),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})
}

// Mirrors parity's Agg_MatchGroup_Indexed: a $gte:0 filter matches every doc.
// The index path materialises every primary id and point-fetches each doc --
// strictly worse than a scan. The benchmark exposes that gap so the planner
// gate that abandons the index over near-full ranges can be tuned.
func BenchmarkIndexLookup_FullRange_10K(b *testing.B) {
	const n = 10_000

	rangeOp := func() *types.Document {
		return must.NotFail(types.NewDocument("$gte", int32(0)))
	}

	b.Run("unindexed_scan", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("grp", rangeOp())),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})

	b.Run("indexed", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{
				{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			},
		}); err != nil {
			b.Fatalf("CreateIndexes: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("grp", rangeOp())),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})
}

// Mirrors parity's FilterRange_10K_Indexed: a 1%-selectivity numeric range.
// The selectivity is what makes the index path beat the full scan.
func BenchmarkIndexLookup_Range_10K(b *testing.B) {
	const n = 10_000
	lo, hi := int32(n/10), int32(n/10+n/100) // [1000, 1100)

	rangeOp := func() *types.Document {
		return must.NotFail(types.NewDocument("$gte", lo, "$lt", hi))
	}

	b.Run("unindexed_scan", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("i", rangeOp())),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})

	b.Run("indexed", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{
				{Name: "i_1", Key: []backends.IndexKeyPair{{Field: "i"}}},
			},
		}); err != nil {
			b.Fatalf("CreateIndexes: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("i", rangeOp())),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})
}

// Mirrors parity's CountDocuments_10K_Indexed. query_and_drain is the old
// handler path (index lookup fetches and decodes every primary value tuple,
// then CountIterator counts); count_fastpath walks just the secondary-index
// range.
func BenchmarkCount_Equality_10K(b *testing.B) {
	const n = 10_000

	b.Run("query_and_drain_indexed", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{
				{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			},
		}); err != nil {
			b.Fatalf("CreateIndexes: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Query(ctx, &backends.QueryParams{
				Filter: must.NotFail(types.NewDocument("grp", int32(i%10))),
			})
			if err != nil {
				b.Fatalf("Query: %v", err)
			}
			drainIter(b, res.Iter)
		}
	})

	b.Run("count_fastpath_indexed", func(b *testing.B) {
		coll, ctx := seedBenchCollection(b, n)
		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{
				{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			},
		}); err != nil {
			b.Fatalf("CreateIndexes: %v", err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			res, err := coll.Count(ctx, &backends.CountParams{
				Filter: must.NotFail(types.NewDocument("grp", int32(i%10))),
			})
			if err != nil {
				b.Fatalf("Count: %v", err)
			}
			if !res.Filtered {
				b.Fatalf("expected Filtered=true on indexed path")
			}
		}
	})
}

// The indexed count short-circuit must return correct counts only when a
// covering single-field index exists, and otherwise decline (Filtered=false)
// so the handler falls back to a scan.
func TestCount_FilteredFastPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	bk := newTestBackend(t)
	db, err := bk.Database("countfast")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	const n = 1000
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, must.NotFail(types.NewDocument(
			"_id", int32(i),
			"i", int32(i),
			"grp", int32(i%10),
			"tag", "row",
		)))
	}
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	res, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument("grp", int32(3))),
	})
	if err != nil {
		t.Fatalf("Count pre-index: %v", err)
	}
	if res.Filtered {
		t.Fatalf("expected Filtered=false without index, got true")
	}

	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			{Name: "i_1", Key: []backends.IndexKeyPair{{Field: "i"}}},
		},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	eqRes, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument("grp", int32(3))),
	})
	if err != nil {
		t.Fatalf("Count equality: %v", err)
	}
	if !eqRes.Filtered {
		t.Errorf("expected Filtered=true for indexed equality")
	}
	if eqRes.Count != 100 {
		t.Errorf("equality count: want 100, got %d", eqRes.Count)
	}

	rangeRes, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument("i",
			must.NotFail(types.NewDocument("$gte", int32(100), "$lt", int32(200))))),
	})
	if err != nil {
		t.Fatalf("Count range: %v", err)
	}
	if !rangeRes.Filtered {
		t.Errorf("expected Filtered=true for indexed range")
	}
	if rangeRes.Count != 100 {
		t.Errorf("range count: want 100, got %d", rangeRes.Count)
	}

	// Compound filter: only one field indexed, so the fast path must decline.
	compoundRes, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument("grp", int32(3), "tag", "row")),
	})
	if err != nil {
		t.Fatalf("Count compound: %v", err)
	}
	if compoundRes.Filtered {
		t.Errorf("expected Filtered=false for compound filter, got true")
	}

	emptyRes, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument()),
	})
	if err != nil {
		t.Fatalf("Count empty: %v", err)
	}
	if !emptyRes.Filtered || emptyRes.Count != int64(n) {
		t.Errorf("empty filter: Filtered=%v Count=%d, want true/%d", emptyRes.Filtered, emptyRes.Count, n)
	}

	tagRes, err := coll.Count(ctx, &backends.CountParams{
		Filter: must.NotFail(types.NewDocument("tag", "row")),
	})
	if err != nil {
		t.Fatalf("Count tag: %v", err)
	}
	if tagRes.Filtered {
		t.Errorf("expected Filtered=false for non-indexed field, got true")
	}
}

// Runs the benchmark loop's code paths but inspects the count, confirming the
// benchmarks see the docs they expect.
func TestIndexLookup_Bench_Sanity(t *testing.T) {
	t.Parallel()
	const n = 1000
	ctx := context.Background()
	b := newTestBackend(t)
	db, err := b.Database("sanity")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	for off := 0; off < n; off += 200 {
		docs := make([]*types.Document, 0, 200)
		for j := 0; j < 200; j++ {
			i := int32(off + j)
			docs = append(docs, must.NotFail(types.NewDocument(
				"_id", i,
				"i", i,
				"grp", i%10,
			)))
		}
		if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
			t.Fatalf("InsertAll: %v", err)
		}
	}
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
			{Name: "i_1", Key: []backends.IndexKeyPair{{Field: "i"}}},
		},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	res, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("grp", int32(3))),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	got := 0
	for {
		_, _, err := res.Iter.Next()
		if err != nil {
			break
		}
		got++
	}
	res.Iter.Close()
	if got != 100 {
		t.Errorf("equality candidate count: want 100, got %d", got)
	}

	rangeRes, err := coll.Query(ctx, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("i",
			must.NotFail(types.NewDocument("$gte", int32(100), "$lt", int32(200))))),
	})
	if err != nil {
		t.Fatalf("Range Query: %v", err)
	}
	got = 0
	for {
		_, _, err := rangeRes.Iter.Next()
		if err != nil {
			break
		}
		got++
	}
	rangeRes.Iter.Close()
	if got != 100 {
		t.Errorf("range candidate count: want 100, got %d", got)
	}
}

// Per-insert cost under a unique index must not scale with collection size
// (P1): the historical implementation scanned and decoded the whole primary
// per insert batch; the probe implementation does one bounded index read per
// row. Pre-seeds collections of different sizes to expose any scaling.
func BenchmarkInsertWithUniqueIndex(b *testing.B) {
	for _, n := range []int{1000, 8000} {
		b.Run(fmt.Sprintf("seed_%d", n), func(b *testing.B) {
			coll, ctx := seedBenchCollection(b, n)
			if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
				Indexes: []backends.IndexInfo{{
					Name:   "by_i_unique",
					Key:    []backends.IndexKeyPair{{Field: "i"}},
					Unique: true,
				}},
			}); err != nil {
				b.Fatalf("CreateIndexes: %v", err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := int32(1_000_000 + i)
				doc := must.NotFail(types.NewDocument("_id", id, "i", id))
				if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{
					Docs:            []*types.Document{doc},
					SkipDurableSync: true,
				}); err != nil {
					b.Fatalf("InsertAll: %v", err)
				}
			}
		})
	}
}
