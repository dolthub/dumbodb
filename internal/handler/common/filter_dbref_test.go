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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFilterComparesDBRefLiterally(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	dbRef := doc("$ref", "x", "$id", doc("$gt", int32(1)))
	sortedDBRef := doc("$id", doc("$gt", int32(1)), "$ref", "x")
	stored := doc("_id", dbRef, "f", sortedDBRef, "g", arr(sortedDBRef))
	other := doc("_id", int32(5), "f", int32(5), "g", arr(int32(5)))

	for _, tc := range []struct {
		name   string
		filter *types.Document
	}{
		{name: "bare _id", filter: doc("_id", dbRef)},
		{name: "_id $eq", filter: doc("_id", doc("$eq", dbRef))},
		{name: "_id $in", filter: doc("_id", doc("$in", arr(dbRef, int32(7))))},
		{name: "_id $nin", filter: doc("_id", doc("$nin", arr(int32(5))))},
		{name: "sorted field", filter: doc("f", sortedDBRef)},
		{name: "sorted field $in", filter: doc("f", doc("$in", arr(sortedDBRef)))},
		{name: "array field element", filter: doc("g", sortedDBRef)},
		{name: "dotted path", filter: doc("g.0", sortedDBRef)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ValidateFilter(tc.filter, nil))
			matched, err := FilterDocument(stored, tc.filter)
			require.NoError(t, err)
			require.True(t, matched)
			matched, err = FilterDocument(other, tc.filter)
			require.NoError(t, err)
			require.False(t, matched)
		})
	}

	_, err := FilterDocument(stored, doc("_id", doc("$in", arr(doc("$gt", int32(1))))))
	require.ErrorContains(t, err, "cannot nest $ under $in")
	_, err = FilterDocument(stored, doc("_id", doc("$ref", "x")))
	require.ErrorContains(t, err, "unknown operator: $")
}

func TestFilterParsesNotOperandAsOperators(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	dbRef := func() *types.Document { return doc("$ref", "x", "$id", int32(1)) }
	stored := doc("_id", int32(3), "f", int32(5), "arr", must.NotFail(types.NewArray(int32(5))))

	for _, tc := range []struct {
		name    string
		filter  func() *types.Document
		message string
	}{
		{"DBRef on field", func() *types.Document { return doc("f", doc("$not", dbRef())) }, "unknown operator: $ref"},
		{"DBRef on _id", func() *types.Document { return doc("_id", doc("$not", dbRef())) }, "unknown operator: $ref"},
		{"DBRef in $elemMatch", func() *types.Document {
			return doc("arr", doc("$elemMatch", doc("$not", dbRef())))
		}, "unknown operator: $ref"},
		{"canonical DBRef", func() *types.Document {
			f := doc("f", doc("$not", dbRef()))
			CanonicalizeFilter(f)
			return f
		}, "unknown operator: $ref"},
		{"plain document", func() *types.Document { return doc("f", doc("$not", doc("a", dbRef()))) }, "unknown operator: a"},
		{"empty", func() *types.Document { return doc("f", doc("$not", doc())) }, "$not argument must be a non-empty object"},
		{"scalar", func() *types.Document { return doc("f", doc("$not", int32(1))) }, "$not argument must be a regex or an object"},
		{"operator then plain field", func() *types.Document {
			return doc("f", doc("$not", doc("$gt", int32(1), "a", int32(1))))
		}, "unknown operator: a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, ValidateFilter(tc.filter(), nil), tc.message)
			_, err := FilterDocument(stored, tc.filter())
			require.ErrorContains(t, err, tc.message)
		})
	}
}
