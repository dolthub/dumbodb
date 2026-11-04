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
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Index ranges are read lazily and sized before they are read
// (dolthub/dumbodb#139): a limit stops the reads, a hint uses the index
// however wide its range, and a range over half the collection is declined
// for a scan after a short read-ahead, with no keys counted as examined.
func TestIndexRangeReads(t *testing.T) {
	var docs []*types.Document
	for i := int32(1); i < 1000; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "g", i%10, "o", i%2)))
	}
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "g", Key: []backends.IndexKeyPair{{Field: "g"}}}, docs...)
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

	type work struct{ keys, docs, returned int64 }
	run := func(params *backends.QueryParams, take int) (work, []int32) {
		t.Helper()
		k0, d0 := queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
		res, err := c.Query(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int32
		for take < 0 || len(ids) < take {
			_, d, err := res.Iter.Next()
			if err != nil {
				break
			}
			ids = append(ids, must.NotFail(d.Get("_id")).(int32))
		}
		res.Iter.Close()
		w := work{queryExecutorCounters.keys.Load() - k0, queryExecutorCounters.docs.Load() - d0, int64(len(ids))}
		return w, ids
	}
	evens := doc("g", doc("$in", must.NotFail(types.NewArray(int32(2), int32(4), int32(6), int32(8)))))
	wide := doc("g", doc("$gte", int32(1)))

	t.Run("limit stops the reads", func(t *testing.T) {
		if w, _ := run(&backends.QueryParams{Filter: evens, LimitHint: 5}, 5); w.keys != 5 || w.docs != 5 {
			t.Fatalf("limited $in read %d keys and %d documents, want 5 and 5", w.keys, w.docs)
		}
	})
	t.Run("repeated $in values yield each document once", func(t *testing.T) {
		_, ids := run(&backends.QueryParams{Filter: doc("g", doc("$in", must.NotFail(types.NewArray(int32(3), int32(3)))))}, -1)
		if len(ids) != 100 || len(slices.Compact(slices.Sorted(slices.Values(ids)))) != 100 {
			t.Fatalf("got %d ids, want 100 distinct", len(ids))
		}
	})
	t.Run("hint reads a wide range from the index", func(t *testing.T) {
		if w, _ := run(&backends.QueryParams{Filter: wide, Hint: "g"}, -1); w.keys != 900 || w.docs != 900 {
			t.Fatalf("hinted wide range read %d keys and %d documents, want 900 and 900", w.keys, w.docs)
		}
	})
	t.Run("hint streams a sorted wide range with a filter field outside the index", func(t *testing.T) {
		params := &backends.QueryParams{Filter: doc("g", doc("$gte", int32(1)), "o", int32(1)), Sort: sortOn("g", 1), LimitHint: 3, Hint: "g"}
		if w, _ := run(params, 3); w.keys != 3 || w.docs != 3 {
			t.Fatalf("hinted sorted wide range read %d keys and %d documents, want 3 and 3", w.keys, w.docs)
		}
	})
	t.Run("wide range scans without reading the index", func(t *testing.T) {
		if w, _ := run(&backends.QueryParams{Filter: wide}, -1); w.keys != 0 || w.docs != 1000 {
			t.Fatalf("wide range read %d keys and %d documents, want a scan: 0 and 1000", w.keys, w.docs)
		}
	})
	t.Run("wide $or scans without reading the index", func(t *testing.T) {
		or := doc("$or", must.NotFail(types.NewArray(wide, doc("g", int32(0)))))
		if w, _ := run(&backends.QueryParams{Filter: or}, -1); w.keys != 0 {
			t.Fatalf("wide $or read %d keys, want a scan", w.keys)
		}
	})
	t.Run("$or beside an indexed field reads that field's index", func(t *testing.T) {
		filter := doc("g", int32(3), "$or", must.NotFail(types.NewArray(doc("o", int32(0)), doc("o", int32(1)))))
		if w, _ := run(&backends.QueryParams{Filter: filter}, -1); w.keys != 100 || w.docs != 100 {
			t.Fatalf("$or beside g read %d keys and %d documents, want 100 and 100", w.keys, w.docs)
		}
	})
	t.Run("nested $or on one index is one scan of its intervals", func(t *testing.T) {
		nested := doc("$or", must.NotFail(types.NewArray(doc("g", int32(1)), doc("$or", must.NotFail(types.NewArray(doc("g", int32(2))))))))
		// The scan also examines the first key past the last interval.
		if w, _ := run(&backends.QueryParams{Filter: nested}, -1); w.keys != 201 || w.docs != 200 {
			t.Fatalf("nested $or read %d keys and %d documents, want 201 and 200", w.keys, w.docs)
		}
	})
}
