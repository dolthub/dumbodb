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

package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestIDValidationMatchesMongodRules(t *testing.T) {
	doc := func(pairs ...any) *Document { return must.NotFail(NewDocument(pairs...)) }
	arr := func(values ...any) *Array { return must.NotFail(NewArray(values...)) }
	for _, tc := range []struct {
		name             string
		id               any
		valid            bool
		replacementValid bool
	}{
		{name: "scalar", id: int32(1), valid: true, replacementValid: true},
		{name: "null", id: Null, valid: true, replacementValid: true},
		{name: "plain document", id: doc("a", int32(1)), valid: true, replacementValid: true},
		{name: "dbref", id: doc("$ref", "x", "$id", int32(7)), valid: true, replacementValid: true},
		{name: "dbref operator-shaped id", id: doc("$ref", "x", "$id", doc("$gt", int32(1))), valid: true},
		{name: "dbref nested dollar in id", id: doc("$ref", "x", "$id", doc("a", doc("$b", int32(1)))), valid: true},
		{name: "dbref with db", id: doc("$ref", "x", "$id", doc("$gt", int32(2)), "$db", "d"), valid: true},
		{name: "dbref dollar in array id", id: doc("$ref", "x", "$id", arr(doc("$x", int32(1)))), valid: true},
		{name: "dbref extra field", id: doc("$ref", "x", "$id", int32(1), "meta", int32(1)), valid: true, replacementValid: true},
		{name: "dbref dollar in extra field", id: doc("$ref", "x", "$id", int32(1), "meta", doc("$y", int32(1)))},
		{name: "nested dbref", id: doc("a", doc("$ref", "x", "$id", doc("$gt", int32(1)))), valid: true},
		{name: "operator", id: doc("$in", arr(int32(1)))},
		{name: "nested operator", id: doc("a", doc("$ne", int32(1)))},
		{name: "dollar in array element", id: doc("a", arr(doc("$x", int32(1))))},
		{name: "dbref keys out of order", id: doc("$id", int32(1), "$ref", "x")},
		{name: "dbref unknown dollar field", id: doc("$ref", "x", "$id", int32(1), "$z", int32(1))},
		{name: "array", id: arr(int32(1))},
		{name: "regex", id: Regex{Pattern: "^x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateID(tc.id)
			insertErr := doc("_id", tc.id).ValidateData()
			replacementErr := ValidateReplacementID(tc.id)
			if tc.replacementValid {
				require.NoError(t, replacementErr)
			} else {
				var ve *ValidationError
				require.ErrorAs(t, replacementErr, &ve)
				require.Contains(t, []ValidationErrorCode{ErrDollarPrefixedID, ErrWrongIDType}, ve.Code())
			}
			if tc.valid {
				require.NoError(t, err)
				require.NoError(t, insertErr)
				return
			}
			require.Error(t, err)
			require.Equal(t, err.Error(), insertErr.Error())
		})
	}
}

func TestValidateReplacementIDNamesFieldPath(t *testing.T) {
	doc := func(pairs ...any) *Document { return must.NotFail(NewDocument(pairs...)) }
	id := doc("a", doc("$ref", "x", "$id", must.NotFail(NewArray(doc("$x", int32(1))))))
	require.EqualError(t, ValidateReplacementID(id),
		"The dollar ($) prefixed field '$x' in '_id.a.$id.0.$x' is not allowed in the context of an update's "+
			"replacement document. Consider using an aggregation pipeline with $replaceWith.")
}
