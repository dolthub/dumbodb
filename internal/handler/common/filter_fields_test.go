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

func TestFilterRootFields(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	for name, tc := range map[string]struct {
		filter *types.Document
		want   []string
		ok     bool
	}{
		"nil":          {nil, nil, true},
		"empty":        {doc(), nil, true},
		"fields":       {doc("a", int32(1), "b.c", doc("$gt", int32(2)), "a.x", "y"), []string{"a", "b"}, true},
		"logical":      {doc("$or", arr(doc("a", int32(1)), doc("$and", arr(doc("c", int32(3)))))), []string{"a", "c"}, true},
		"comment":      {doc("$comment", "x", "a", int32(1)), []string{"a"}, true},
		"expr":         {doc("a", int32(1), "$expr", doc("$eq", arr("$b", int32(1)))), nil, false},
		"where":        {doc("$where", "this.a == 1"), nil, false},
		"text":         {doc("$text", doc("$search", "x")), nil, false},
		"nested expr":  {doc("$or", arr(doc("$expr", true))), nil, false},
		"bad or value": {doc("$or", "x"), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := FilterRootFields(tc.filter)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
