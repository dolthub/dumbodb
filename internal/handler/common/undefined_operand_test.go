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

func TestFilterRejectsUndefinedOperands(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	target := doc("_id", int32(1), "f", int32(1))
	for _, tc := range []struct {
		name   string
		filter *types.Document
		msg    string
	}{
		{"equality", doc("f", types.Undefined), "cannot compare to undefined"},
		{"equality on a missing field", doc("missing", types.Undefined), "cannot compare to undefined"},
		{"$eq", doc("f", doc("$eq", types.Undefined)), "cannot compare to undefined"},
		{"$ne", doc("f", doc("$ne", types.Undefined)), "cannot compare to undefined"},
		{"$lt after a failing operator", doc("f", doc("$gt", int32(5), "$lt", types.Undefined)), "cannot compare to undefined"},
		{"$in", doc("f", doc("$in", arr(int32(1), types.Undefined))), "Cannot insert undefined into InListData"},
		{"$nin", doc("f", doc("$nin", arr(types.Undefined))), "Cannot insert undefined into InListData"},
		{"$all", doc("f", doc("$all", arr(types.Undefined))), "cannot compare to undefined"},
		{"$not", doc("f", doc("$not", doc("$eq", types.Undefined))), "cannot compare to undefined"},
		{"$elemMatch", doc("f", doc("$elemMatch", doc("$gte", types.Undefined))), "cannot compare to undefined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FilterDocument(target, tc.filter)
			require.ErrorContains(t, err, tc.msg)
		})
	}
	for _, filter := range []*types.Document{
		doc("f", doc("$exists", types.Undefined)),
		doc("f", arr(types.Undefined)),
		doc("f", doc("x", types.Undefined)),
	} {
		_, err := FilterDocument(target, filter)
		require.NoError(t, err, "filter %v", filter)
	}
}
