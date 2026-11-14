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

// The positional $ after an $elemMatch on the array names the first element
// that satisfies the $elemMatch expression.
func TestFindFirstMatchingIndexElemMatch(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	em := func(expr *types.Document) *types.Document { return doc("$elemMatch", expr) }
	for name, tc := range map[string]struct {
		path   string
		target *types.Document
		filter *types.Document
		want   int
	}{
		"scalars":           {"a", doc("a", arr(int32(1), int32(5))), doc("a", em(doc("$gt", int32(3)))), 1},
		"documents":         {"a", doc("a", arr(doc("x", int32(1)), doc("x", int32(5)))), doc("a", em(doc("x", doc("$gt", int32(3))))), 1},
		"dotted path":       {"b.c", doc("b", doc("c", arr(int32(1), int32(5), int32(7)))), doc("b.c", em(doc("$gt", int32(3)))), 1},
		"beside $size":      {"a", doc("a", arr(int32(1), int32(5), int32(9))), doc("a", doc("$elemMatch", doc("$gt", int32(3)), "$size", int32(3))), 1},
		"array element":     {"a", doc("a", arr(int32(2), arr(int32(1)), int32(6))), doc("a", em(doc("$gt", types.MinKey, "$type", "array"))), 1},
		"no element":        {"a", doc("a", arr(int32(5))), doc("a", em(doc("$gt", types.MaxKey))), -1},
		"without elemMatch": {"a", doc("a", arr(int32(1), int32(5))), doc("a", int32(5)), 1},
	} {
		got, err := findFirstMatchingIndex(tc.path, tc.target, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: index %d, want %d", name, got, tc.want)
		}
	}
}
