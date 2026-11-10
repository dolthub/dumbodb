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
	"hash/fnv"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestClusteredScan checks _id ranges and sorts read the primary map in key
// order, counting the document past the far bound examined as MongoDB does.
func TestClusteredScan(t *testing.T) {
	c := newOrScanCollection(t) // _id 1..300
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	in := func(vs ...any) *types.Document { return doc("$in", must.NotFail(types.NewArray(vs...))) }
	for _, tc := range []struct {
		name    string
		params  backends.QueryParams
		stage   string
		ids     []int32
		docs    int64
		ordered bool
	}{
		{"range", backends.QueryParams{Filter: doc("_id", doc("$gte", int32(10), "$lt", int32(13)))}, "CLUSTERED_IXSCAN", []int32{10, 11, 12}, 4, false},
		{"range to the end", backends.QueryParams{Filter: doc("_id", doc("$gt", int32(298)))}, "CLUSTERED_IXSCAN", []int32{299, 300}, 2, false},
		{"range past the end", backends.QueryParams{Filter: doc("_id", doc("$gte", "a"))}, "CLUSTERED_IXSCAN", nil, 0, false},
		{"range descending", backends.QueryParams{Filter: doc("_id", doc("$gt", int32(10), "$lte", int32(12))), Sort: doc("_id", int32(-1))}, "CLUSTERED_IXSCAN", []int32{12, 11}, 3, true},
		{"in", backends.QueryParams{Filter: doc("_id", in(int32(5), int32(3), int32(7)))}, "CLUSTERED_IXSCAN", []int32{3, 4, 5, 6, 7}, 6, false},
		{"point beside another field", backends.QueryParams{Filter: doc("_id", int32(5), "k", int32(5))}, "CLUSTERED_IXSCAN", []int32{5}, 2, false},
		{"sort", backends.QueryParams{Sort: doc("_id", int32(-1)), Limit: 3}, "COLLSCAN", []int32{300, 299, 298}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, examined := backends.WithExaminedCounts(context.Background())
			res, plan, err := c.query(ctx, &tc.params)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Iter.Close()
			var ids []int32
			for {
				_, d, err := res.Iter.Next()
				if err != nil {
					break
				}
				ids = append(ids, must.NotFail(d.Get("_id")).(int32))
			}
			if plan.stage != tc.stage || res.Sorted != tc.ordered {
				t.Errorf("plan %s sorted %t, want %s sorted %t", plan.stage, res.Sorted, tc.stage, tc.ordered)
			}
			// The scan returns candidates; the handler applies the rest of the filter.
			if !slices.Equal(ids, tc.ids) {
				t.Errorf("ids %v, want %v", ids, tc.ids)
			}
			if examined.Docs() != tc.docs {
				t.Errorf("examined %d docs, want %d", examined.Docs(), tc.docs)
			}
		})
	}
}

func TestKeyBytesToRecordIDIsFNV64a(t *testing.T) {
	for _, key := range [][]byte{nil, {0x2B, 0x02}, []byte("\x3Cabc\x00"), make([]byte, 40)} {
		h := fnv.New64a()
		_, _ = h.Write(key)
		if got, want := keyBytesToRecordID(key), int64(h.Sum64()); got != want {
			t.Errorf("% X: %d, want %d", key, got, want)
		}
	}
	if n := testing.AllocsPerRun(100, func() { keyBytesToRecordID([]byte{0x2B, 0x02}) }); n != 0 {
		t.Errorf("%v allocations, want 0", n)
	}
}

// TestClusteredInEmptyFieldName checks an _id $in holding a document with an
// empty field name is a value, not an operator.
func TestClusteredInEmptyFieldName(t *testing.T) {
	c := newOrScanCollection(t)
	ctx := context.Background()
	id := must.NotFail(types.NewDocument("", int32(1)))
	if _, err := c.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{must.NotFail(types.NewDocument("_id", id))}}); err != nil {
		t.Fatal(err)
	}
	filter := must.NotFail(types.NewDocument("_id", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(id))))))
	res, _, err := c.query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Iter.Close()
	if _, d, err := res.Iter.Next(); err != nil || d == nil {
		t.Fatalf("want the document, got %v, %v", d, err)
	}
}

// TestClusteredInRangeStopsWhenCanceled: encoding a small Decimal128 is slow at
// extreme exponents, so a large _id $in stops once its context ends.
func TestClusteredInRangeStopsWhenCanceled(t *testing.T) {
	d, err := primitive.ParseDecimal128("1.234567890123456789012345678901234E-6065")
	if err != nil {
		t.Fatal(err)
	}
	hi, lo := d.GetBytes()
	values := make([]any, 100_000)
	for i := range values {
		values[i] = types.Decimal128{H: hi, L: lo + uint64(i)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, _, ok := clusteredInRange(ctx, must.NotFail(types.NewArray(values...))); ok {
		t.Fatal("a canceled $in range was built")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a canceled $in of %d values took %v", len(values), elapsed)
	}
}
