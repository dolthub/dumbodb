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

package projection

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCheckPathCollisions(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	for _, tc := range []struct {
		spec *types.Document
		msg  string
	}{
		{doc("a", int32(1), "a.b", int32(1)), "Path collision at a.b remaining portion b"},
		{doc("a.b", int32(1), "a", int32(1)), "Path collision at a"},
		{doc("a", doc("b", int32(1)), "a.b", int32(1)), "Path collision at a.b"},
	} {
		require.ErrorContains(t, CheckPathCollisions(tc.spec, "", "projection"), tc.msg, "%v", tc.spec)
	}
	require.NoError(t, CheckPathCollisions(doc("a.b", int32(1), "a.c", int32(1), "ab", int32(1)), "", "projection"))
}
