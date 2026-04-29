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
	"log/slog"
	"os"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// newBenchBackend mirrors newTestBackend (which takes *testing.T) for use
// inside benchmarks. Kept private to this file so it doesn't pollute the
// shared dolt-backend test surface.
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

// seedBenchCollection creates a 10K-document collection with the same shape
// the parity harness uses (small docs with i, grp, tag, payload). It returns
// the collection plus a function that creates the secondary index on demand.
//
// The seed is shared between indexed and unindexed variants so the docs paid
// for once during benchmark setup aren't counted in the per-iteration timer.
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

	// Insert in batches of 1000 to avoid one giant InsertAll call dominating
	// the seed phase.
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

// drainIter consumes the backend iterator and returns the count of documents
// produced. Mirrors what the handler does after Query — its cost dominates
// the indexed path, so the benchmark must include it.
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

// BenchmarkIndexLookup_Equality_10K matches the shape of
// parity's BenchmarkFind_FilterEq_10K_Indexed: filter is a single equality
// constraint on a 10-way bucketed field, so each query returns ~1K of 10K.
// Compares the un-indexed scan path against the secondary-index path.
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

// BenchmarkIndexLookup_Range_10K mirrors the parity FilterRange_10K_Indexed
// benchmark: a 1%-selectivity numeric range. The selectivity is what makes
// the index path interesting — the un-indexed scan still touches every doc.
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

// Sanity-check that the benchmarks see the docs they expect. Goes through
// the same code paths as the benchmark loop body but inspects the count.
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
	// Expected: ~100 (1000/10), exact since seed is deterministic.
	if got != 100 {
		t.Errorf("equality candidate count: want 100, got %d", got)
	}

	// Range: 100..200 → 100 matches.
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
