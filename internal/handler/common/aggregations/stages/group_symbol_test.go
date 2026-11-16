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

package stages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestGroupMapSymbolAndJavaScriptKeys(t *testing.T) {
	t.Parallel()

	m := newGroupMap(nil, nil)
	keys := []any{
		types.Symbol("a"), types.Symbol("b"), "a", types.JavaScript("a"), types.JavaScript("b"),
		must.NotFail(types.NewDocument("k", "b")), must.NotFail(types.NewDocument("k", types.Symbol("b"))),
	}
	var slots []int
	for _, k := range keys {
		slots = append(slots, m.index(k))
	}
	require.Equal(t, []int{0, 1, 0, 2, 3, 4, 4}, slots)
	require.Equal(t, types.Symbol("a"), m.docs[0].groupID)
}
