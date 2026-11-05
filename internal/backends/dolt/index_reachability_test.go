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
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Secondary index maps are reachable by chunk walks (dolthub/dumbodb#190):
// GC keeps them, and push and clone carry them.

func kRange(lo, hi int32) *types.Document {
	return must.NotFail(types.NewDocument("k", must.NotFail(types.NewDocument("$gte", lo, "$lt", hi))))
}

// hintedCount reads filter through index k_1 and returns the documents
// returned and index keys read. Not for parallel tests: it reads the
// process-wide executor counters.
func hintedCount(t *testing.T, ctx context.Context, coll backends.Collection, filter *types.Document) (docs, keys int64) {
	t.Helper()
	keysBefore := queryExecutorCounters.keys.Load()
	defer func() { keys = queryExecutorCounters.keys.Load() - keysBefore }()
	res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter, Hint: "k_1"})
	require.NoError(t, err)
	defer res.Iter.Close()
	for {
		if _, _, err := res.Iter.Next(); err != nil {
			break
		}
		docs++
	}
	return docs, 0
}

func TestSecondaryIndexesSurviveGC(t *testing.T) {
	for _, mode := range []string{"default", "full"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			open := func() *Backend {
				be, err := newBackend(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), true, 0, 0)
				require.NoError(t, err)
				return be
			}
			read := func(be *Backend, label string) {
				ctx := ctxWithSession(t, be, "gc-index-"+mode+"-"+label)
				coll := must.NotFail(must.NotFail(be.Database("gcidx")).Collection("items"))
				docs, keys := hintedCount(t, ctx, coll, kRange(100, 150))
				require.Equal(t, int64(50), docs, label)
				require.Equal(t, int64(50), keys, label)
			}

			be := open()
			ctx := ctxWithSession(t, be, "gc-index-"+mode)
			db := must.NotFail(be.Database("gcidx"))
			require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))
			coll := must.NotFail(db.Collection("items"))
			var docs []*types.Document
			for i := int32(0); i < 2000; i++ {
				docs = append(docs, must.NotFail(types.NewDocument("_id", i, "k", i, "pad", fmt.Sprintf("%0200d", i))))
			}
			_, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
			require.NoError(t, err)
			_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{Name: "k_1", Key: []backends.IndexKeyPair{{Field: "k"}}}}})
			require.NoError(t, err)

			read(be, "before")
			res, err := be.DumboDBGC(ctx, &backends.GCParams{DBName: "gcidx", Mode: mode})
			require.NoError(t, err)
			require.Less(t, res.ChunksAfter, res.ChunksBefore, "GC swept nothing; the test proves nothing")
			read(be, "after GC")
			be.Close()

			reopened := open()
			defer reopened.Close()
			read(reopened, "after reopen")
		})
	}
}

func TestCloneCarriesSecondaryIndexes(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	remoteURL := "file://" + t.TempDir()
	for i := int32(0); i < 300; i++ {
		insertDoc(t, b, "src", "coll", mustDoc(t, "_id", i, "k", i))
	}
	coll := must.NotFail(must.NotFail(b.Database("src")).Collection("coll"))
	_, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{Name: "k_1", Key: []backends.IndexKeyPair{{Field: "k"}}}}})
	require.NoError(t, err)
	commitDB(t, b, "src", "c1")
	_, err = b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL})
	require.NoError(t, err)
	_, err = b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"})
	require.NoError(t, err)
	_, err = b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	require.NoError(t, err)

	cloned := must.NotFail(must.NotFail(b.Database("clonedb")).Collection("coll"))
	docs, keys := hintedCount(t, ctx, cloned, kRange(100, 110))
	require.Equal(t, int64(10), docs)
	require.Equal(t, int64(10), keys)
}

// A table written before dolthub/dumbodb#190 holds an AddressMap in
// secondary_indexes; it is refused rather than read.
func TestSecondaryIndexesRefuseAddressMapLayout(t *testing.T) {
	b := newTestBackend(t)
	insertDoc(t, b, "olddb", "coll", mustDoc(t, "_id", int32(1)))
	state := mustDB(t, b, "olddb")
	am := must.NotFail(prolly.NewEmptyAddressMap(state.ns))
	_, err := secondaryIndexesFromBytes(state.ns, []byte(tree.ValueFromNode(am.Node()).(dolttypes.SerialMessage)))
	require.ErrorContains(t, err, "recreate the database")
}

// The definition keeps the key order of a partial filter and collation as
// the user gave them.
func TestIndexDefinitionKeepsKeyOrder(t *testing.T) {
	partial := must.NotFail(types.NewDocument("b", must.NotFail(types.NewDocument("$gt", int32(1))), "a", must.NotFail(types.NewDocument("$exists", true))))
	collation := must.NotFail(types.NewDocument("strength", int32(2), "locale", "en"))
	def, err := encodeIndexDefinition(backends.IndexInfo{
		Name: "p", Key: []backends.IndexKeyPair{{Field: "a"}}, PartialFilterExpression: partial, Collation: collation,
	})
	require.NoError(t, err)
	entry, err := resolveIndexEntry(def, hash.Of([]byte("root")))
	require.NoError(t, err)
	require.Equal(t, []string{"b", "a"}, entry.info.PartialFilterExpression.Keys())
	require.Equal(t, []string{"strength", "locale"}, entry.info.Collation.Keys())
	match, err := entry.info.MatchesPartialFilter(must.NotFail(types.NewDocument("a", int32(1), "b", int32(2))))
	require.NoError(t, err)
	require.True(t, match)
	require.True(t, slices.Equal(entry.info.Key, []backends.IndexKeyPair{{Field: "a"}}))
}
