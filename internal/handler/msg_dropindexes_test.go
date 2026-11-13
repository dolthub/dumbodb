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

func TestProcessDropIndexOptions_KeyPatternLength(t *testing.T) {
	existing := []backends.IndexInfo{
		{Name: "_id_", Key: []backends.IndexKeyPair{{Field: "_id"}}},
		{Name: "a_1_b_1", Key: []backends.IndexKeyPair{{Field: "a"}, {Field: "b"}}},
		{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
	}

	for name, tc := range map[string]struct {
		pattern *types.Document
		want    string
	}{
		"shorter than an index": {pattern: must.NotFail(types.NewDocument("a", int32(1)))},
		"longer than an index":  {pattern: must.NotFail(types.NewDocument("x", int32(1), "y", int32(1)))},
		"exact compound":        {pattern: must.NotFail(types.NewDocument("a", int32(1), "b", int32(1))), want: "a_1_b_1"},
		"exact single":          {pattern: must.NotFail(types.NewDocument("x", int32(1))), want: "x_1"},
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
