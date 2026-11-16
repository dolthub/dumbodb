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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Validating many update paths takes time linear in their number: a
// quadratic conflict check takes minutes on these.
func TestValidateUpdateOperatorsManyPaths(t *testing.T) {
	const n = 20_000
	set, rename := types.MakeDocument(n), types.MakeDocument(n)
	for i := range n {
		set.Set(fmt.Sprintf("s%d.x", i), int32(1))
		rename.Set(fmt.Sprintf("r%d", i), fmt.Sprintf("t%d", i))
	}
	update := must.NotFail(types.NewDocument("$set", set, "$rename", rename))

	start := time.Now()
	require.NoError(t, ValidateUpdateOperators("update", update))
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestUpdatePathsConflict(t *testing.T) {
	paths := newUpdatePaths()
	paths.add("a.b")
	paths.add("c")
	for path, want := range map[string]string{"a": "a", "a.b": "a.b", "a.b.c": "a.b", "c.d": "c", "ab": "", "a.c": ""} {
		at, ok := paths.conflict(path)
		require.Equal(t, want != "", ok, path)
		require.Equal(t, want, at, path)
	}
}

func TestUpdatePathsArrayFilterConflict(t *testing.T) {
	paths := newUpdatePaths()
	paths.add("a.$[]")
	paths.add("b.$[e].x")
	paths.add("c.0.x")
	for path, want := range map[string]string{
		"a.0": "a", "a.foo": "a", "a.$": "a", "a.$[f]": "", "b.0.y": "b", "b.$[].y": "", "b.$[e].y": "",
		"c.$[].y": "c", "c.1": "", "c.0.$[]": "c.0",
	} {
		at, ok := paths.conflict(path)
		require.Equal(t, want != "", ok, path)
		require.Equal(t, want, at, path)
	}
}
