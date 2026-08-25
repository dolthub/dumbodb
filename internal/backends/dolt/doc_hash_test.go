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
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func docHashBackend(t *testing.T) (context.Context, backends.Collection) {
	t.Helper()

	dir, err := os.MkdirTemp("", "dolt-doc-hash-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	ctx := context.Background()
	b := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	t.Cleanup(b.Close)

	_, err = b.getOrOpenDB(ctx, "hashdb", true)
	require.NoError(t, err)

	db, err := b.Database("hashdb")
	require.NoError(t, err)
	coll, err := db.Collection("col")
	require.NoError(t, err)

	return ctx, coll
}

func docHashDoc(t *testing.T, pairs ...any) *types.Document {
	t.Helper()

	doc, err := types.NewDocument(pairs...)
	require.NoError(t, err)
	return doc
}

// The hash a write reports is the hash of the canonical stored bytes, so a
// client that hashes the same document reaches the same answer.
func TestDocHashMatchesStoredBytes(t *testing.T) {
	ctx, coll := docHashBackend(t)

	doc := docHashDoc(t, "_id", int64(1), "a", "x")
	res, err := coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs:            []*types.Document{doc},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	require.Len(t, res.DocHashes, 1)

	stored, err := docToBSON(doc)
	require.NoError(t, err)
	require.Equal(t, docContentHash(stored).String(), res.DocHashes[0])
	require.Len(t, res.DocHashes[0], 32, "hashes render as 32-character base32, like commit hashes")
}

// Field order on the wire does not reach storage: the codec sorts keys at
// every level, so the same content has one hash.
func TestDocHashIgnoresFieldOrder(t *testing.T) {
	first, err := docToBSON(docHashDoc(t, "_id", int64(1), "b", int64(2), "a", int64(1)))
	require.NoError(t, err)
	second, err := docToBSON(docHashDoc(t, "_id", int64(1), "a", int64(1), "b", int64(2)))
	require.NoError(t, err)

	require.Equal(t, docContentHash(first), docContentHash(second))
}

// The hash follows the content: it moves when a field changes and returns to
// its earlier value when the earlier content is written again.
func TestDocHashFollowsContent(t *testing.T) {
	ctx, coll := docHashBackend(t)

	ins, err := coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs:            []*types.Document{docHashDoc(t, "_id", int64(1), "a", "x")},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	original := ins.DocHashes[0]

	upd, err := coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs:            []*types.Document{docHashDoc(t, "_id", int64(1), "a", "y")},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), upd.Updated)
	require.Len(t, upd.DocHashes, 1)
	require.NotEqual(t, original, upd.DocHashes[0])

	back, err := coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs:            []*types.Document{docHashDoc(t, "_id", int64(1), "a", "x")},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	require.Equal(t, original, back.DocHashes[0], "identical content must hash identically")
}

// Distinct documents hash distinctly, and a document stored out of band (over
// the tuple-builder threshold) is hashed like any other: the hash covers the
// stored bytes, not the storage shape.
func TestDocHashCoversEveryStorageShape(t *testing.T) {
	ctx, coll := docHashBackend(t)

	small := docHashDoc(t, "_id", int64(1), "a", "x")
	large := docHashDoc(t, "_id", int64(2), "a", strings.Repeat("q", 8*1024))

	res, err := coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs:            []*types.Document{small, large},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	require.Len(t, res.DocHashes, 2)
	require.NotEqual(t, res.DocHashes[0], res.DocHashes[1])

	for _, h := range res.DocHashes {
		require.NotEmpty(t, h)
	}

	largeBytes, err := docToBSON(large)
	require.NoError(t, err)
	require.Equal(t, docContentHash(largeBytes).String(), res.DocHashes[1])
}

// Nothing is computed for a caller that did not ask.
func TestDocHashAbsentUnlessRequested(t *testing.T) {
	ctx, coll := docHashBackend(t)

	ins, err := coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs: []*types.Document{docHashDoc(t, "_id", int64(1), "a", "x")},
	})
	require.NoError(t, err)
	require.Nil(t, ins.DocHashes)

	upd, err := coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{docHashDoc(t, "_id", int64(1), "a", "y")},
	})
	require.NoError(t, err)
	require.Nil(t, upd.DocHashes)
}

// A document that matches nothing leaves its slot empty, so the caller can
// tell which of its documents were written.
func TestDocHashEmptyForUnmatchedUpdate(t *testing.T) {
	ctx, coll := docHashBackend(t)

	_, err := coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs: []*types.Document{docHashDoc(t, "_id", int64(1), "a", "x")},
	})
	require.NoError(t, err)

	upd, err := coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{
			docHashDoc(t, "_id", int64(404), "a", "z"),
			docHashDoc(t, "_id", int64(1), "a", "y"),
		},
		ReturnDocHashes: true,
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), upd.Updated)
	require.Len(t, upd.DocHashes, 2)
	require.Empty(t, upd.DocHashes[0])
	require.NotEmpty(t, upd.DocHashes[1])
}
