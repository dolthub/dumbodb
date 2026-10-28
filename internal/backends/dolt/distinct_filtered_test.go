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

func TestDistinctScanFilteredOnKey(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{Name: "v", Key: []backends.IndexKeyPair{{Field: "v"}}})
	for i, v := range []any{int32(1), 1.0, int32(2), int32(150), "a", int32(-3), int32(2)} {
		if err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", int32(i+1), "v", v))); err != nil {
			t.Fatal(err)
		}
	}
	lt100 := must.NotFail(types.NewDocument("v", must.NotFail(types.NewDocument("$lt", int32(100)))))
	res, err := c.(backends.DistinctScanner).DistinctScan(ctx, &backends.DistinctParams{Key: "v", Filter: lt100})
	if err != nil || res == nil {
		t.Fatalf("DistinctScan = %v, %v", res, err)
	}
	var got []float64
	for _, v := range res.Values {
		switch n := v.(type) {
		case int32:
			got = append(got, float64(n))
		case float64:
			got = append(got, n)
		default:
			t.Fatalf("unexpected value %v", v)
		}
	}
	slices.Sort(got)
	if want := []float64{-3, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("values = %v, want %v", got, want)
	}

	other := must.NotFail(types.NewDocument("w", int32(1)))
	if res, err := c.(backends.DistinctScanner).DistinctScan(ctx, &backends.DistinctParams{Key: "v", Filter: other}); err != nil || res != nil {
		t.Fatalf("a filter on another field must decline, got %v, %v", res, err)
	}
	if err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", int32(100), "v", must.NotFail(types.NewArray(int32(5), int32(500)))))); err != nil {
		t.Fatal(err)
	}
	if res, err := c.(backends.DistinctScanner).DistinctScan(ctx, &backends.DistinctParams{Key: "v", Filter: lt100}); err != nil || res != nil {
		t.Fatalf("a multikey index must decline, got %v, %v", res, err)
	}
}
