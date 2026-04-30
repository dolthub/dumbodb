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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Profiling benchmarks for do-upta. Each benchmark isolates one operation at
// the backend layer so cpuprofile output attributes time to DumboDB code
// rather than wire-protocol/driver framing.
//
// Run individual benchmarks with:
//
//   go test -run=^$ -bench=BenchmarkProfile_FindFilterEq_50K_Indexed \
//     -benchtime=20x -cpuprofile=cpu.out -memprofile=mem.out \
//     ./internal/backends/dolt
//
// Then:
//   go tool pprof -top -cum cpu.out

// BenchmarkProfile_FindFilterEq_50K_Indexed mirrors the parity benchmark
// find_filter_eq_50k_indexed: a single equality on the indexed `grp` field
// over a 50K-doc collection. With `grp = i%10` each query returns ~5K of
// 50K rows. The 50% range gate in tryIndexLookup admits this (~10% of the
// collection), so the index path runs.
func BenchmarkProfile_FindFilterEq_50K_Indexed(b *testing.B) {
	const n = 50_000
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
}

// BenchmarkProfile_CountDocuments_10K_Indexed mirrors count_documents_10k:
// CountDocuments({grp: x}) on an indexed field. Routes through Count, which
// should hit the indexed-count fast path (RangeCount over the secondary
// index, no document fetches).
func BenchmarkProfile_CountDocuments_10K_Indexed(b *testing.B) {
	const n = 10_000
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
			b.Fatalf("expected indexed count fast path (Filtered=true)")
		}
	}
}

// BenchmarkProfile_CountDocuments_50K_Indexed mirrors count_documents_50k.
func BenchmarkProfile_CountDocuments_50K_Indexed(b *testing.B) {
	const n = 50_000
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
			b.Fatalf("expected indexed count fast path (Filtered=true)")
		}
	}
}

// BenchmarkProfile_AggMatchGroup_50K_Indexed mirrors agg_match_group_indexed.
// Pattern: [{$match: {grp: x}}, {$group: {_id: "$grp", c: {$sum: 1}}}].
// The handler runs $match through Query and $group as in-memory aggregation,
// so the backend cost of this benchmark is identical to FindFilterEq above —
// the per-doc deserialisation work is what feeds the group stage. Profiling
// the Query path captures the dominant cost.
func BenchmarkProfile_AggMatchGroup_50K_Indexed(b *testing.B) {
	const n = 50_000
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
		// Simulate $group {_id: "$grp", c: {$sum: 1}} — read every doc the
		// match produced, look up grp, count it. Mirrors the handler's
		// stage processing without the full pipeline plumbing.
		count := int64(0)
		for {
			_, doc, err := res.Iter.Next()
			if err != nil {
				break
			}
			if _, gerr := doc.Get("grp"); gerr != nil {
				b.Fatalf("doc missing grp: %v", gerr)
			}
			count++
		}
		res.Iter.Close()
		if count == 0 {
			b.Fatalf("group received zero docs")
		}
	}
}

// BenchmarkProfile_Distinct_50K_Indexed mirrors distinct (indexed). Calls
// the DistinctScan fast path directly. The seed has only 10 unique `grp`
// values so the index walk emits 10 primary lookups out of 50K index keys.
func BenchmarkProfile_Distinct_50K_Indexed(b *testing.B) {
	const n = 50_000
	coll, ctx := seedBenchCollection(b, n)
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
		},
	}); err != nil {
		b.Fatalf("CreateIndexes: %v", err)
	}
	ds, ok := coll.(backends.DistinctScanner)
	if !ok {
		b.Fatalf("collection is not a DistinctScanner — index path unavailable")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := ds.DistinctScan(ctx, &backends.DistinctParams{Key: "grp"})
		if err != nil {
			b.Fatalf("DistinctScan: %v", err)
		}
		if res == nil {
			b.Fatalf("DistinctScan declined — index path not used")
		}
		if len(res.Values) != 10 {
			b.Fatalf("DistinctScan: want 10 values, got %d", len(res.Values))
		}
	}
}
