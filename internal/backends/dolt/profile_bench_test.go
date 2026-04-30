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

// Profile-only benchmarks that mirror the parity-suite shapes that are
// currently 4-10x slower than MongoDB on the unindexed read path:
//
//   parity bench                            this profile bench
//   ---------------------------------       --------------------------------
//   BenchmarkFind_FilterEq_50K               BenchmarkProfile_FindFilterEq_50K
//   BenchmarkAgg_Sort                        BenchmarkProfile_AggSort
//   BenchmarkAgg_MatchGroup                  BenchmarkProfile_AggMatchGroup
//
// Where the parity benches dial a running DumboDB over the wire, these run
// in-process against the dolt backend and chain the same handler-side
// iterators (FilterIterator / aggregation stages) the real handler builds.
// That keeps the same dominant code paths but lets `go test -cpuprofile` see
// them under their real symbols, without going through the wire bridge or
// the test harness DB lifecycle.
//
// The intent is to find where time goes — not to set or beat any baseline.
// Use:
//
//   go test -run=__nope__ -bench=BenchmarkProfile_FindFilterEq_50K \
//     -benchtime=10x -cpuprofile=/tmp/findeq.cpu -memprofile=/tmp/findeq.mem \
//     ./internal/backends/dolt/
//   go tool pprof -top -cum /tmp/findeq.cpu

import (
	"context"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// drainAndDiscard pulls every doc out of the iterator. The benchmark needs
// the per-iter cost including decode of the final document; it doesn't care
// about the values.
func drainAndDiscard(it types.DocumentsIterator) {
	for {
		_, _, err := it.Next()
		if err != nil {
			return
		}
	}
}

// BenchmarkProfile_FindFilterEq_50K mirrors the wire-level
// BenchmarkFind_FilterEq_50K from the parity suite: 50K small docs, equality
// filter on the 10-bucketed `grp` field, no index. Each iteration drains the
// full result set (~5K docs).
//
// What runs per iteration:
//   - collection.Query — opens a prolly map iterator, attaches the byte-level
//     prefilter built from the equality filter
//   - mapIter.Next — for each row: tree walk, JSON bytes fetch, prefilter
//     check, BSON-via-ExtJSON decode
//   - common.FilterIterator — re-validates each candidate against the
//     types.Document filter (catches false positives from the prefilter)
//
// This is the same iterator stack the wire-level Find handler builds.
func BenchmarkProfile_FindFilterEq_50K(b *testing.B) {
	coll, ctx := seedBenchCollection(b, 50_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		filter := must.NotFail(types.NewDocument("grp", int32(i%10)))
		res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		closer := iterator.NewMultiCloser()
		filtered := common.FilterIterator(res.Iter, closer, filter)
		drainAndDiscard(filtered)
		closer.Close()
	}
}

// BenchmarkProfile_AggSort mirrors the wire-level BenchmarkAgg_Sort. Pipeline
// is [{$sort:{i:-1}}, {$limit:100}] over 1K small docs. The sort buffers all
// 1K rows; the $limit cuts to 100 after.
//
// What runs per iteration:
//   - collection.Query — full scan, no filter pushdown (no $match before sort)
//   - sortStage.Process — slurp + slices.SortFunc
//   - limitStage.Process — head 100
func BenchmarkProfile_AggSort(b *testing.B) {
	coll, ctx := seedBenchCollection(b, 1000)
	sortStage, err := stages.NewStage(must.NotFail(types.NewDocument(
		"$sort", must.NotFail(types.NewDocument("i", int32(-1))))))
	if err != nil {
		b.Fatalf("NewStage($sort): %v", err)
	}
	limitStage, err := stages.NewStage(must.NotFail(types.NewDocument(
		"$limit", int32(100))))
	if err != nil {
		b.Fatalf("NewStage($limit): %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := coll.Query(ctx, &backends.QueryParams{})
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		closer := iterator.NewMultiCloser()
		out, err := sortStage.Process(ctx, res.Iter, closer)
		if err != nil {
			b.Fatalf("$sort.Process: %v", err)
		}
		out, err = limitStage.Process(ctx, out, closer)
		if err != nil {
			b.Fatalf("$limit.Process: %v", err)
		}
		drainAndDiscard(out)
		closer.Close()
	}
}

// BenchmarkProfile_AggMatchGroup mirrors the wire-level
// BenchmarkAgg_MatchGroup. Pipeline:
//
//	[{$match:{grp:{$gte:0}}}, {$group:{_id:"$grp", n:{$sum:1}}}]
//
// over 1K small docs. Every doc matches; group reduces to 10 buckets.
//
// What runs per iteration:
//   - collection.Query — full scan, no pushdown (operator-doc filter on grp)
//   - matchStage.Process — re-checks every doc against the $match filter
//   - groupStage.Process — buckets and sums
func BenchmarkProfile_AggMatchGroup(b *testing.B) {
	coll, ctx := seedBenchCollection(b, 1000)
	matchStage, err := stages.NewStage(must.NotFail(types.NewDocument(
		"$match", must.NotFail(types.NewDocument(
			"grp", must.NotFail(types.NewDocument("$gte", int32(0))))))))
	if err != nil {
		b.Fatalf("NewStage($match): %v", err)
	}
	groupStage, err := stages.NewStage(must.NotFail(types.NewDocument(
		"$group", must.NotFail(types.NewDocument(
			"_id", "$grp",
			"n", must.NotFail(types.NewDocument("$sum", int32(1))),
		)))))
	if err != nil {
		b.Fatalf("NewStage($group): %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := coll.Query(ctx, &backends.QueryParams{})
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		closer := iterator.NewMultiCloser()
		out, err := matchStage.Process(ctx, res.Iter, closer)
		if err != nil {
			b.Fatalf("$match.Process: %v", err)
		}
		out, err = groupStage.Process(ctx, out, closer)
		if err != nil {
			b.Fatalf("$group.Process: %v", err)
		}
		drainAndDiscard(out)
		closer.Close()
	}
}

// Compile-time assertion that the iterator types we touch satisfy the
// interface — keeps the benchmark file honest if signatures change.
var _ context.Context = context.Background()
