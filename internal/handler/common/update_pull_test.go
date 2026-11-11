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

func TestPullConditionQueriesEmbeddedDocuments(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	for _, tc := range []struct {
		name      string
		condition *types.Document
		elem      any
		match     bool
	}{
		{"field operator on matching document", doc("a", doc("$gt", int32(1))), doc("a", int32(3)), true},
		{"field operator on other document", doc("a", doc("$gt", int32(1))), doc("a", int32(0)), false},
		{"field $not", doc("a", doc("$not", doc("$gt", int32(1)))), doc("b", int32(1)), true},
		{"literal field matches a wider document", doc("a", int32(1)), doc("a", int32(1), "b", int32(2)), true},
		{"$nor", doc("$nor", arr(doc("a", int32(1)))), doc("a", int32(0)), true},
		{"empty matches any document", doc(), doc("a", int32(1)), true},
		{"query never matches a scalar", doc("a", int32(1)), int32(1), false},
		{"element operator applies to a scalar", doc("$gt", int32(1)), int32(5), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, err := pullConditionMatches(tc.elem, tc.condition)
			require.NoError(t, err)
			require.Equal(t, tc.match, match)
		})
	}
}
