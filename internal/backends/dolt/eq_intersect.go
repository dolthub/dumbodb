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
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// equalityIntersection intersects a field condition holding $eq beside other
// comparisons, as mongod intersects their index bounds: on a field holding no
// array it matches the equality's value alone, or nothing. ok is false for
// any other condition.
func equalityIntersection(v any) (reduced any, empty, ok bool) {
	ops, isDoc := v.(*types.Document)
	if !isDoc || ops.Len() < 2 || !ops.Has("$eq") {
		return nil, false, false
	}
	eq, isEq := indexEqualityValue(must.NotFail(types.NewDocument("$eq", must.NotFail(ops.Get("$eq")))))
	if !isEq {
		return nil, false, false
	}
	meets := true
	for _, op := range ops.Keys() {
		operand := must.NotFail(ops.Get(op))
		switch op {
		case "$eq", "$ne":
			if op == "$eq" {
				meets = meets && sameValue(eq, operand)
			} else {
				meets = meets && !sameValue(eq, operand)
			}
		case "$gt", "$gte", "$lt", "$lte":
			if !comparable(eq, operand) {
				meets = false
				continue
			}
			c := types.Compare(eq, operand)
			switch op {
			case "$gt":
				meets = meets && c == types.Greater
			case "$gte":
				meets = meets && c != types.Less
			case "$lt":
				meets = meets && c == types.Less
			case "$lte":
				meets = meets && c != types.Greater
			}
		case "$in", "$nin":
			arr, isArr := operand.(*types.Array)
			if !isArr {
				return nil, false, false
			}
			found := false
			for i := 0; i < arr.Len(); i++ {
				if sameValue(eq, must.NotFail(arr.Get(i))) {
					found = true
				}
			}
			meets = meets && found == (op == "$in")
		default:
			return nil, false, false
		}
	}
	if !meets {
		return nil, true, true
	}
	return must.NotFail(types.NewDocument("$eq", eq)), false, true
}

// filterNeverMatches reports whether a field of filter must equal a value it
// must also differ from, which no document meets: mongod plans it as EOF.
func filterNeverMatches(filter *types.Document) bool {
	for _, k := range filter.Keys() {
		if strings.HasPrefix(k, "$") {
			continue
		}
		ops, ok := must.NotFail(filter.Get(k)).(*types.Document)
		if !ok || !ops.Has("$eq") {
			continue
		}
		eq := must.NotFail(ops.Get("$eq"))
		if ne, err := ops.Get("$ne"); err == nil && sameValue(eq, ne) {
			return true
		}
		if nin, err := ops.Get("$nin"); err == nil {
			if arr, isArr := nin.(*types.Array); isArr {
				for i := 0; i < arr.Len(); i++ {
					if sameValue(eq, must.NotFail(arr.Get(i))) {
						return true
					}
				}
			}
		}
	}
	return false
}

// reducedForIndex returns filter with each condition on a key field of idx
// that holds no array intersected (equalityIntersection), and whether one of
// them matches nothing.
func reducedForIndex(filter *types.Document, idx backends.IndexInfo) (*types.Document, bool) {
	var out *types.Document
	for i, kp := range idx.Key {
		if idx.KeyMultikey(i) {
			continue
		}
		v, err := filter.Get(kp.Field)
		if err != nil {
			continue
		}
		reduced, empty, ok := equalityIntersection(v)
		if !ok {
			continue
		}
		if empty {
			return filter, true
		}
		if out == nil {
			out = filter.DeepCopy()
		}
		out.Set(kp.Field, reduced)
	}
	if out == nil {
		return filter, false
	}
	return out, false
}

// sameValue reports whether a and b are equal values of one type class.
func sameValue(a, b any) bool {
	return comparable(a, b) && types.Compare(a, b) == types.Equal
}

// comparable reports whether a and b fall in one BSON type class, which a
// comparison's bounds stay within.
func comparable(a, b any) bool {
	return types.SameTypeClass(a, b)
}

// equalitiesAlone returns filter with each condition equalityIntersection
// can intersect replaced by its equality, which picks the index the
// intersected bounds read.
func equalitiesAlone(filter *types.Document) *types.Document {
	var out *types.Document
	for _, k := range filter.Keys() {
		ops, ok := must.NotFail(filter.Get(k)).(*types.Document)
		if !ok {
			continue
		}
		if _, _, ok = equalityIntersection(ops); !ok {
			continue
		}
		if out == nil {
			out = filter.DeepCopy()
		}
		out.Set(k, must.NotFail(types.NewDocument("$eq", must.NotFail(ops.Get("$eq")))))
	}
	if out == nil {
		return filter
	}
	return out
}
