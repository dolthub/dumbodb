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

package operators

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestOperatorsTreatUndefinedAsNullish(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	undef := doc("$literal", types.Undefined)
	missingF := doc("_id", int32(1))
	undefinedF := doc("_id", int32(1), "f", types.Undefined)
	nullF := doc("_id", int32(1), "f", types.Null)
	for _, tc := range []struct {
		name string
		expr *types.Document
		doc  *types.Document
		want any
	}{
		{"$eq missing undefined", doc("$eq", arr("$f", undef)), missingF, true},
		{"$eq undefined undefined", doc("$eq", arr("$f", undef)), undefinedF, true},
		{"$eq null undefined", doc("$eq", arr("$f", undef)), nullF, false},
		{"$cmp missing undefined", doc("$cmp", arr("$f", undef)), missingF, int32(0)},
		{"$lt undefined null", doc("$lt", arr(undef, types.Null)), missingF, true},
		{"$eq missing null", doc("$eq", arr("$f", types.Null)), missingF, false},
		{"$lt missing null", doc("$lt", arr("$f", types.Null)), missingF, true},
		{"$cmp missing null", doc("$cmp", arr("$f", types.Null)), missingF, int32(-1)},
		{"$cmp null missing", doc("$cmp", arr(types.Null, "$f")), missingF, int32(1)},
		{"$eq stored null null", doc("$eq", arr("$f", types.Null)), nullF, true},
		{"$lt missing minKey", doc("$lt", arr("$f", types.MinKey)), missingF, false},
		{"$eq missing missing", doc("$eq", arr("$f", "$g")), missingF, true},
		{"$ifNull", doc("$ifNull", arr(undef, "x")), missingF, "x"},
		{"$ifNull stored", doc("$ifNull", arr("$f", "x")), undefinedF, "x"},
		{"$add", doc("$add", arr(int32(1), undef)), missingF, types.Null},
		{"$concat", doc("$concat", arr("a", undef)), missingF, types.Null},
		{"$toBool", doc("$toBool", undef), missingF, types.Null},
		{"$toString", doc("$toString", undef), missingF, types.Null},
		{"$cond", doc("$cond", arr(undef, "t", "f")), missingF, "f"},
		{"$and", doc("$and", arr(undef)), missingF, false},
		{"$not", doc("$not", arr(undef)), missingF, true},
		{"$type literal", doc("$type", undef), missingF, "undefined"},
		{"$type stored", doc("$type", "$f"), undefinedF, "undefined"},
		{"$type decimal", doc("$type", types.Decimal128{H: 0x3040000000000000}), missingF, "decimal"},
		{"$type minKey", doc("$type", types.MinKey), missingF, "minKey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewOperator(tc.expr)
			require.NoError(t, err)
			got, err := op.Process(tc.doc)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
