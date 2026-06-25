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

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestQuery_regex_dotall verifies that {$options: "s"} (dotall mode) makes "."
// match newline characters in the target string. (DumboDBFull)
func TestQuery_regex_dotall(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("text", "hello\nworld")),
		d(e("_id", int32(2)), e("text", "hello world")),
		d(e("_id", int32(3)), e("text", "goodbye")),
	)

	ctx := context.Background()

	// Without "s" flag, "." does not match newline  -- only doc 2 matches.
	cursor, err := coll.Find(ctx,
		d(e("text", d(e("$regex", "hello.world")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(2), results[0][0].Value)

	// With "s" flag, "." matches newline  -- docs 1 and 2 both match.
	cursor2, err := coll.Find(ctx,
		d(e("text", d(e("$regex", "hello.world"), e("$options", "s")))),
	)
	require.NoError(t, err)
	defer cursor2.Close(ctx)

	var results2 []bson.D
	require.NoError(t, cursor2.All(ctx, &results2))
	require.Len(t, results2, 2, "dotall mode must match newline in '.'")

	ids := make([]int32, 0, len(results2))
	for _, doc := range results2 {
		ids = append(ids, doc[0].Value.(int32))
	}
	assert.ElementsMatch(t, []int32{1, 2}, ids)
}
