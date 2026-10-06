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

package cursor

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func newTestCursor(t *testing.T, docs []*types.Document) (*Registry, *Cursor) {
	t.Helper()
	r := NewRegistry(slog.Default())
	c := r.NewCursor(context.Background(), iterator.Values(iterator.ForSlice(docs)), &NewParams{Type: Normal})
	return r, c
}

func docsWithPad(n, padBytes int) []*types.Document {
	pad := strings.Repeat("x", padBytes)
	docs := make([]*types.Document, n)
	for i := range docs {
		docs[i] = must.NotFail(types.NewDocument("_id", int32(i), "pad", pad))
	}
	return docs
}

func ids(docs []*types.Document) []int32 {
	out := make([]int32, len(docs))
	for i, d := range docs {
		out[i] = must.NotFail(d.Get("_id")).(int32)
	}
	return out
}

func TestNextBatch_CountLimit(t *testing.T) {
	r, c := newTestCursor(t, docsWithPad(5, 0))

	for _, want := range []struct {
		ids  []int32
		done bool
	}{
		{[]int32{0, 1}, false},
		{[]int32{2, 3}, false},
		{[]int32{4}, true},
	} {
		batch, done, err := c.NextBatch(2, types.MaxDocumentLen)
		require.NoError(t, err)
		require.Equal(t, want.ids, ids(batch))
		require.Equal(t, want.done, done)
	}
	require.Nil(t, r.Get(c.ID), "an exhausted normal cursor is removed")
}

func TestNextBatch_SizeLimitCarriesOverflowDocument(t *testing.T) {
	_, c := newTestCursor(t, docsWithPad(60, 600*1024))

	first, done, err := c.NextBatch(101, types.MaxDocumentLen)
	require.NoError(t, err)
	require.False(t, done)
	require.Len(t, first, 27, "27 documents of 600KB fit in 16MiB")

	second, done, err := c.NextBatch(101, types.MaxDocumentLen)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, int32(27), ids(second)[0], "the document that did not fit starts the next batch")
	require.Len(t, second, 27)

	third, done, err := c.NextBatch(101, types.MaxDocumentLen)
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, third, 6)
}

func TestNextBatch_OversizedDocumentStillMakesProgress(t *testing.T) {
	_, c := newTestCursor(t, docsWithPad(3, 1000))

	for i := int32(0); i < 3; i++ {
		batch, _, err := c.NextBatch(10, 100)
		require.NoError(t, err)
		require.Equal(t, []int32{i}, ids(batch))
	}
}

func TestNextBatch_UnlimitedAndZero(t *testing.T) {
	_, c := newTestCursor(t, docsWithPad(300, 0))

	empty, done, err := c.NextBatch(0, types.MaxDocumentLen)
	require.NoError(t, err)
	require.False(t, done)
	require.Empty(t, empty, "batchSize 0 returns an empty batch and keeps the cursor open")

	all, done, err := c.NextBatch(-1, types.MaxDocumentLen)
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, all, 300)
}
