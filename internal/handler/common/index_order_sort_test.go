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

// TestSortInIndexOrderTypeArrayBesideRange: a two-sided range beside a $type
// naming arrays still bounds the walk, and sorting by it must not fail.
func TestSortInIndexOrderTypeArrayBesideRange(t *testing.T) {
	for _, typ := range []any{"array", must.NotFail(types.NewArray("array", "number"))} {
		docs := []*types.Document{
			must.NotFail(types.NewDocument("_id", int32(1), "f", must.NotFail(types.NewArray(int32(2), int32(3))))),
			must.NotFail(types.NewDocument("_id", int32(2), "f", must.NotFail(types.NewArray(int32(0), int32(4))))),
		}
		filter := must.NotFail(types.NewDocument("f", must.NotFail(types.NewDocument("$gt", int32(1), "$lt", int32(5), "$type", typ))))
		pattern := must.NotFail(types.NewDocument("f", int32(1)))
		if err := sortDocumentsInIndexOrder(docs, pattern, filter, nil); err != nil {
			t.Fatalf("$type %v: %v", typ, err)
		}
		if id := must.NotFail(docs[0].Get("_id")); id != int32(1) {
			t.Errorf("$type %v: first _id = %v, want 1 (first key in the range)", typ, id)
		}
	}
}
