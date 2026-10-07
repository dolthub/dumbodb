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

package clientconn

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
)

// The check runs on every OP_MSG before authentication, so its cost must be
// linear in the number of fields. A quadratic walk over 200k fields takes tens
// of seconds; a linear one takes milliseconds.
func TestCommandDuplicateFieldIsLinearInFieldCount(t *testing.T) {
	const fieldCount = 200_000

	pairs := make([]any, 0, fieldCount*2)
	for i := range fieldCount {
		pairs = append(pairs, "k"+strconv.Itoa(i), types.Null)
	}
	wide, err := types.NewDocument(pairs...)
	require.NoError(t, err)

	cmd, err := types.NewDocument("ping", int32(1), "x", wide)
	require.NoError(t, err)

	start := time.Now()
	_, found := commandDuplicateField(cmd)
	elapsed := time.Since(start)

	require.False(t, found)
	require.Less(t, elapsed, 2*time.Second, "duplicate-field check took %s for %d fields", elapsed, fieldCount)
}

func TestCommandDuplicateFieldStillFindsNestedDuplicate(t *testing.T) {
	inner, err := types.NewDocument("a", int32(1), "a", int32(2))
	require.NoError(t, err)
	cmd, err := types.NewDocument("find", "c", "filter", inner)
	require.NoError(t, err)

	path, found := commandDuplicateField(cmd)
	require.True(t, found)
	require.Equal(t, "filter.a", path)
}
