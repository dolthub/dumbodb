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

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestProcessDropIndexOptions_KeyPattern(t *testing.T) {
	existing := []backends.IndexInfo{
		{Name: "_id_", Key: []backends.IndexKeyPair{{Field: "_id"}}},
		{Name: "a_1_b_1", Key: []backends.IndexKeyPair{{Field: "a"}, {Field: "b"}}},
		{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
		{Name: "h_hashed", Key: []backends.IndexKeyPair{{Field: "h", Hashed: true}}},
		{Name: "s_2dsphere", Key: []backends.IndexKeyPair{{Field: "s", Geo2DSphere: true}}},
		{Name: "p_2d", Key: []backends.IndexKeyPair{{Field: "p", Geo2D: true}}},
		{Name: "k_1_t_text_u_text", Key: []backends.IndexKeyPair{{Field: "k"}, {Field: "t", Text: true}, {Field: "u", Text: true}}},
	}
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

	for name, tc := range map[string]struct {
		pattern *types.Document
		want    string
	}{
		"shorter than an index":  {pattern: doc("a", int32(1))},
		"longer than an index":   {pattern: doc("x", int32(1), "y", int32(1))},
		"exact compound":         {pattern: doc("a", int32(1), "b", int32(1)), want: "a_1_b_1"},
		"exact single":           {pattern: doc("x", int32(1)), want: "x_1"},
		"descending":             {pattern: doc("x", int32(-1))},
		"hashed by ascending":    {pattern: doc("h", int32(1))},
		"ascending by hashed":    {pattern: doc("x", "hashed")},
		"hashed":                 {pattern: doc("h", "hashed"), want: "h_hashed"},
		"2dsphere by ascending":  {pattern: doc("s", int32(1))},
		"2dsphere by 2d":         {pattern: doc("s", "2d")},
		"2dsphere":               {pattern: doc("s", "2dsphere"), want: "s_2dsphere"},
		"2d by ascending":        {pattern: doc("p", int32(1))},
		"2d":                     {pattern: doc("p", "2d"), want: "p_2d"},
		"text by ascending":      {pattern: doc("k", int32(1), "t", int32(1), "u", int32(1))},
		"text by its fields":     {pattern: doc("k", int32(1), "t", "text", "u", "text")},
		"text by stored pattern": {pattern: doc("k", int32(1), "_fts", "text", "_ftsx", int32(1)), want: "k_1_t_text_u_text"},
	} {
		t.Run(name, func(t *testing.T) {
			toDrop, dropAll, err := processDropIndexOptions("dropIndexes", "db.c", tc.pattern, existing)
			require.False(t, dropAll)
			if tc.want == "" {
				var cmdErr *handlererrors.CommandError
				require.ErrorAs(t, err, &cmdErr)
				require.Equal(t, handlererrors.ErrIndexNotFound, cmdErr.Code())
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{tc.want}, toDrop)
		})
	}
}
