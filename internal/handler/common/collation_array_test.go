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

package common

import (
	"strconv"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A query's collation reaches strings inside arrays, nested arrays included.
func TestFilterCollationArrayElements(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	cmp := collation.Parse(doc("locale", "en", "strength", int32(2))).Comparator()
	target := doc("_id", int32(1), "a", arr(int32(1), "N", arr("Q")), "d", arr(doc("x", "N")))
	for name, tc := range map[string]struct {
		filter *types.Document
		want   bool
	}{
		"equality":             {doc("a", "n"), true},
		"$ne":                  {doc("a", doc("$ne", "n")), false},
		"$gte":                 {doc("a", doc("$gte", "n")), true},
		"$lt":                  {doc("a", doc("$lt", "m")), false},
		"$in":                  {doc("a", doc("$in", arr("x", "n"))), true},
		"$nin":                 {doc("a", doc("$nin", arr("n"))), false},
		"$all":                 {doc("a", doc("$all", arr("n", int32(1)))), true},
		"nested array":         {doc("a", arr("q")), true},
		"$elemMatch nested":    {doc("a", doc("$elemMatch", doc("$eq", arr("q")))), true},
		"embedded doc via dot": {doc("d.x", "n"), true},
	} {
		got, err := FilterDocumentColl(target, tc.filter, cmp)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: matched %v, want %v", name, got, tc.want)
		}
	}
}

// Under a collation, comparing arrays walks them element by element: a
// document of 20,000 small arrays against a query array of 20,000 strings
// does not copy the query array per element.
func TestFilterCollationArrayElementsWide(t *testing.T) {
	const n = 20_000
	elems := types.MakeArray(n)
	strs := types.MakeArray(n)
	for i := 0; i < n; i++ {
		elems.Append(must.NotFail(types.NewArray(int32(1))))
		strs.Append("s" + strconv.Itoa(i))
	}
	target := must.NotFail(types.NewDocument("_id", int32(1), "a", elems))
	cmp := collation.Parse(must.NotFail(types.NewDocument("locale", "en", "strength", int32(2)))).Comparator()
	for name, filter := range map[string]*types.Document{
		"equality": must.NotFail(types.NewDocument("a", strs)),
		"$all":     must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$all", must.NotFail(types.NewArray(strs)))))),
	} {
		start := time.Now()
		if _, err := FilterDocumentColl(target, filter, cmp); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("%s took %v", name, elapsed)
		}
	}
}
