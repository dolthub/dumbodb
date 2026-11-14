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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A partial index whose condition is {$exists: true} serves a query whose
// condition on the field cannot match a missing field.
func TestPartialIndexServesExists(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	exists := doc("opt", doc("$exists", true))
	for _, tc := range []struct {
		name   string
		filter *types.Document
		pfe    *types.Document
		want   bool
	}{
		{"$exists repeated", doc("opt", doc("$exists", true)), exists, true},
		{"$exists 1", doc("opt", doc("$exists", int32(1))), doc("opt", doc("$exists", int32(1))), true},
		{"equality", doc("opt", int32(5)), exists, true},
		{"comparison", doc("opt", doc("$gt", int32(5))), exists, true},
		{"$in without null", doc("opt", doc("$in", arr(int32(1), int32(2)))), exists, true},
		{"$in with null", doc("opt", doc("$in", arr(int32(1), types.Null))), exists, false},
		{"null equality", doc("opt", types.Null), exists, false},
		{"$ne", doc("opt", doc("$ne", int32(5))), exists, false},
		{"field missing from filter", doc("m", int32(5)), exists, false},
		{"$exists false in pfe", doc("opt", doc("$exists", true)), doc("opt", doc("$exists", false)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, partialIndexServes(context.Background(), tc.filter, backends.IndexInfo{PartialFilterExpression: tc.pfe}, nil))
		})
	}
}
