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
	"math"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFilterImpliesPartialRange(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	lt100 := doc("grp", doc("$lt", int32(100)))
	between := doc("grp", doc("$gte", int32(10), "$lt", int32(100)))
	for _, tc := range []struct {
		name   string
		filter *types.Document
		pfe    *types.Document
		want   bool
	}{
		{"same range", lt100, lt100, true},
		{"tighter upper", doc("grp", doc("$lt", int32(50))), lt100, true},
		{"inclusive at the bound", doc("grp", doc("$lte", int32(100))), lt100, false},
		{"exclusive at an inclusive bound", doc("grp", doc("$lt", int32(100))), doc("grp", doc("$lte", int32(100))), true},
		{"looser upper", doc("grp", doc("$lt", int32(150))), lt100, false},
		{"equality inside", doc("grp", int32(5)), lt100, true},
		{"$eq inside", doc("grp", doc("$eq", 99.5)), lt100, true},
		{"equality outside", doc("grp", int32(100)), lt100, false},
		{"mixed numeric types", doc("grp", doc("$lt", int64(99))), doc("grp", doc("$lte", 99.0)), true},
		{"string equality against numeric range", doc("grp", "a"), lt100, false},
		{"missing lower bound", doc("grp", doc("$lt", int32(50))), between, false},
		{"both bounds", doc("grp", doc("$gt", int32(10), "$lte", int32(20))), between, true},
		{"lower too loose", doc("grp", doc("$gte", int32(9), "$lt", int32(20))), between, false},
		{"string range", doc("s", doc("$gte", "b", "$lt", "c")), doc("s", doc("$gte", "a")), true},
		{"field missing from filter", doc("k", int32(1)), lt100, false},
		{"unsupported operator in filter", doc("grp", doc("$in", must.NotFail(types.NewArray(int32(1))))), lt100, false},
		{"unsupported operator in pfe", lt100, doc("grp", doc("$exists", true)), false},
		{"mixed brackets in pfe", lt100, doc("grp", doc("$lt", int32(100), "$gt", "a")), false},
		{"NaN", doc("grp", doc("$lt", math.NaN())), lt100, false},
		{"scalar equality", doc("grp", int32(5)), doc("grp", int32(5)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterImpliesPartial(tc.filter, tc.pfe, true); got != tc.want {
				t.Fatalf("filterImpliesPartial(%v, %v) = %v, want %v", tc.filter, tc.pfe, got, tc.want)
			}
		})
	}
}

func TestFilterImpliesPartialCollatedStringRange(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	for _, tc := range []struct {
		name   string
		filter *types.Document
		pfe    *types.Document
		want   bool
	}{
		{"string range", doc("s", doc("$lt", "Z")), doc("s", doc("$lt", "a")), false},
		{"numeric range", doc("grp", doc("$lt", int32(50))), doc("grp", doc("$lt", int32(100))), true},
		{"string equality", doc("s", "a"), doc("s", "a"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterImpliesPartial(tc.filter, tc.pfe, false); got != tc.want {
				t.Fatalf("filterImpliesPartial(%v, %v) = %v, want %v", tc.filter, tc.pfe, got, tc.want)
			}
		})
	}
}
