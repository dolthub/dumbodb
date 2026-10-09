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

package dolt

import (
	"context"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestReadStoredBytesReusing(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()

	var values []val.Tuple
	var want [][]byte
	var docs []*types.Document
	for _, size := range []int{50_000, 10, 20_000, 120_000} {
		doc := must.NotFail(types.NewDocument("_id", int32(size), "pad", strings.Repeat("x", size)))
		stored, err := docToBSON(doc)
		require.NoError(t, err)
		v, err := buildValue(ctx, ns, stored, hash.Hash{})
		require.NoError(t, err)
		values = append(values, v)
		want = append(want, stored)
	}

	var buf []byte
	for i, v := range values {
		var stored []byte
		var err error
		stored, buf, err = readStoredBytesReusing(ctx, ns, v, buf)
		require.NoError(t, err)
		require.Equal(t, want[i], stored)

		doc, err := bsonToDoc(stored)
		require.NoError(t, err)
		docs = append(docs, doc)
	}

	for i, doc := range docs {
		expected, err := bsonToDoc(want[i])
		require.NoError(t, err)
		require.Equal(t, types.Equal, types.Compare(expected, doc), "document %d changed after the buffer was reused", i)
	}
}
