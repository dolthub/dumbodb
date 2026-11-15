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
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// An array element of an $elemMatch compares with MinKey and MaxKey, which
// order below and above every type.
func TestFilterElemMatchArrayElementMinMaxKey(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	target := doc("_id", int32(1), "a", arr(arr("x", int32(-1))))

	for name, tc := range map[string]struct {
		cond *types.Document
		want bool
	}{
		"$gt MinKey":         {doc("$gt", types.MinKey), true},
		"$gte MinKey":        {doc("$gte", types.MinKey), true},
		"$lt MinKey":         {doc("$lt", types.MinKey), false},
		"$lte MinKey":        {doc("$lte", types.MinKey), false},
		"$lt MaxKey":         {doc("$lt", types.MaxKey), true},
		"$lte MaxKey":        {doc("$lte", types.MaxKey), true},
		"$gt MaxKey":         {doc("$gt", types.MaxKey), false},
		"$gte MaxKey":        {doc("$gte", types.MaxKey), false},
		"$not $gt MinKey":    {doc("$not", doc("$gt", types.MinKey)), false},
		"$gt number":         {doc("$gt", int32(-5)), false},
		"$gt MinKey, $lt 5":  {doc("$gt", types.MinKey, "$lt", int32(5)), false},
		"$gt [], $lt MaxKey": {doc("$gt", arr(), "$lt", types.MaxKey), true},
	} {
		got, err := FilterDocument(target, doc("a", doc("$elemMatch", tc.cond)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: matched %v, want %v", name, got, tc.want)
		}
	}
}

// An empty $elemMatch matches an array holding a document or an array, so its
// negations keep exactly the other documents.
func TestFilterEmptyElemMatch(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	empty := doc("$elemMatch", doc())
	targets := []any{arr(int32(1)), arr(doc("b", int32(1))), arr(), arr(arr()), int32(1)}

	for name, tc := range map[string]struct {
		filter *types.Document
		want   []bool
	}{
		"$elemMatch":      {doc("a", empty), []bool{false, true, false, true, false}},
		"$not $elemMatch": {doc("a", doc("$not", empty)), []bool{true, false, true, false, true}},
		"$nor $elemMatch": {doc("$nor", arr(doc("a", empty))), []bool{true, false, true, false, true}},
	} {
		for i, a := range targets {
			got, err := FilterDocument(doc("_id", int32(i+1), "a", a), tc.filter)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got != tc.want[i] {
				t.Errorf("%s: _id %d matched %v, want %v", name, i+1, got, tc.want[i])
			}
		}
	}
}
