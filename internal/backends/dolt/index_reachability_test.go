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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Secondary index maps are reachable by chunk walks (dolthub/dumbodb#190):
// GC keeps them, and push and clone carry them.

// indexLayout is a collection whose secondary_indexes take one of the shapes
// the layout allows: a single inline definition, a definition too large to
// inline, or enough indexes that the map splits into several nodes.
type indexLayout struct {
	name   string
	fields []string
	docs   int32
	shape  func(t *testing.T, ctx context.Context, s secondaryIndexes)
}

var indexLayouts = []indexLayout{
	{name: "inline", fields: []string{"k"}, docs: 2000, shape: requireInlineDefinitions},
	{name: "out-of-line definition", fields: []string{strings.Repeat("f", 40000)}, docs: 300, shape: requireOutOfLineDefinition},
	{name: "multi-node map", fields: manyFields(60, 600), docs: 300, shape: requireMultiNodeMap},
}

func definitionValues(t *testing.T, ctx context.Context, s secondaryIndexes) []any {
	t.Helper()
	iter, err := s.m.IterAll(ctx)
	require.NoError(t, err)
	var out []any
	for {
		_, v, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		def, ok, err := secondaryIndexValReadDesc.GetBytesAdaptiveValue(ctx, 0, s.ns, v)
		require.NoError(t, err)
		require.True(t, ok)
		out = append(out, def)
	}
}

func requireInlineDefinitions(t *testing.T, ctx context.Context, s secondaryIndexes) {
	for _, def := range definitionValues(t, ctx, s) {
		require.IsType(t, []byte(nil), def)
	}
}

func requireOutOfLineDefinition(t *testing.T, ctx context.Context, s secondaryIndexes) {
	for _, def := range definitionValues(t, ctx, s) {
		require.IsType(t, (*val.ByteArray)(nil), def)
	}
}

func requireMultiNodeMap(t *testing.T, ctx context.Context, s secondaryIndexes) {
	require.Greater(t, s.m.Node().Level(), 0, "secondary_indexes fits in one node")
}

func manyFields(n, size int) []string {
	fields := make([]string, n)
	for i := range fields {
		fields[i] = fmt.Sprintf("%03d", i) + strings.Repeat("f", size-3)
	}
	return fields
}

func indexName(i int) string { return fmt.Sprintf("i%03d", i) }

// populate inserts l.docs documents into dbName.coll and indexes each field.
func (l indexLayout) populate(t *testing.T, ctx context.Context, b *Backend, dbName string) {
	t.Helper()
	db := must.NotFail(b.Database(dbName))
	coll := must.NotFail(db.Collection("coll"))
	var docs []*types.Document
	for i := int32(0); i < l.docs; i++ {
		doc := must.NotFail(types.NewDocument("_id", i, "pad", fmt.Sprintf("%0200d", i)))
		for _, f := range l.fields {
			doc.Set(f, i)
		}
		docs = append(docs, doc)
	}
	_, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
	require.NoError(t, err)
	var infos []backends.IndexInfo
	for i, f := range l.fields {
		infos = append(infos, backends.IndexInfo{Name: indexName(i), Key: []backends.IndexKeyPair{{Field: f}}})
	}
	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: infos})
	require.NoError(t, err)
}

// requireReadable reads every index map of dbName.coll in full, then queries
// the first index end to end.
func (l indexLayout) requireReadable(t *testing.T, ctx context.Context, b *Backend, dbName, label string) {
	t.Helper()
	state := mustDB(t, b, dbName)
	indexes := secondaryIndexesOnDisk(t, ctx, b, dbName, defaultBranch, "coll")
	l.shape(t, ctx, indexes)
	var names []string
	require.NoError(t, indexes.IterAll(ctx, func(name string, entry *resolvedIndexEntry) error {
		names = append(names, name)
		m, err := openIndexMap(ctx, state.ns, entry.mapRoot)
		if err != nil {
			return err
		}
		iter, err := m.IterAll(ctx)
		if err != nil {
			return err
		}
		var n int32
		for {
			if _, _, err := iter.Next(ctx); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			n++
		}
		require.Equal(t, l.docs, n, "%s: entries in %s", label, name)
		return nil
	}))
	require.Len(t, names, len(l.fields), label)

	coll := must.NotFail(must.NotFail(b.Database(dbName)).Collection("coll"))
	filter := must.NotFail(types.NewDocument(l.fields[0], must.NotFail(types.NewDocument("$gte", int32(100), "$lt", int32(150)))))
	docs, keys := hintedCount(t, ctx, coll, indexName(0), filter)
	require.Equal(t, int64(50), docs, label)
	require.Equal(t, int64(50), keys, label)
}

// hintedCount reads filter through index hint and returns the documents
// returned and index keys read. Not for parallel tests: it reads the
// process-wide executor counters.
func hintedCount(t *testing.T, ctx context.Context, coll backends.Collection, hint string, filter *types.Document) (docs, keys int64) {
	t.Helper()
	keysBefore := queryExecutorCounters.keys.Load()
	res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter, Hint: hint})
	require.NoError(t, err)
	for {
		if _, _, err := res.Iter.Next(); err != nil {
			break
		}
		docs++
	}
	res.Iter.Close()
	return docs, queryExecutorCounters.keys.Load() - keysBefore
}

func TestSecondaryIndexesSurviveGC(t *testing.T) {
	for _, l := range indexLayouts {
		for _, mode := range []string{"default", "full"} {
			t.Run(l.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				open := func() *Backend {
					be, err := newBackend(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), true, 0, 0)
					require.NoError(t, err)
					return be
				}
				be := open()
				ctx := ctxWithSession(t, be, "gc-index")
				require.NoError(t, must.NotFail(be.Database("gcidx")).CreateCollection(ctx, &backends.CreateCollectionParams{Name: "coll"}))
				l.populate(t, ctx, be, "gcidx")
				l.requireReadable(t, ctx, be, "gcidx", "before GC")

				res, err := be.DumboDBGC(ctx, &backends.GCParams{DBName: "gcidx", Mode: mode})
				require.NoError(t, err)
				require.Less(t, res.ChunksAfter, res.ChunksBefore, "GC swept nothing; the test proves nothing")
				l.requireReadable(t, ctx, be, "gcidx", "after GC")
				be.Close()

				reopened := open()
				defer reopened.Close()
				l.requireReadable(t, ctxWithSession(t, reopened, "gc-index-reopen"), reopened, "gcidx", "after reopen")
			})
		}
	}
}

func TestCloneCarriesSecondaryIndexes(t *testing.T) {
	for _, l := range indexLayouts {
		t.Run(l.name, func(t *testing.T) {
			ctx := context.Background()
			b := newTestBackend(t)
			remoteURL := "file://" + t.TempDir()
			l.populate(t, ctx, b, "src")
			commitDB(t, b, "src", "c1")
			_, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL})
			require.NoError(t, err)
			_, err = b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"})
			require.NoError(t, err)
			_, err = b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
			require.NoError(t, err)
			l.requireReadable(t, ctx, b, "clonedb", "clone")
		})
	}
}

func TestBackupCarriesSecondaryIndexes(t *testing.T) {
	for _, l := range indexLayouts {
		t.Run(l.name, func(t *testing.T) {
			ctx := context.Background()
			b := newSyncTestBackend(t)
			l.populate(t, ctx, b, "src")
			url := "file://" + filepath.Join(t.TempDir(), "bk")
			_, err := b.DumboDBBackup(ctx, &backends.BackupParams{DBName: "src", Action: "sync-url", URL: url})
			require.NoError(t, err)
			_, err = b.DumboDBBackup(ctx, restoreParams(url, "restored", false))
			require.NoError(t, err)
			l.requireReadable(t, ctx, b, "restored", "restore")
		})
	}
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

// A data dir whose admin database holds the old layout is refused at
// startup: auth reads users from admin, so serving it would fail every login.
func TestStartupRefusesAdminAddressMapLayout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	open := func() (*Backend, error) {
		return newBackend(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), true, 0, 0)
	}
	be, err := open()
	require.NoError(t, err)
	insertDoc(t, be, "admin", "legacy", mustDoc(t, "_id", int32(1)))
	state := mustDB(t, be, "admin")
	state.mu.Lock()
	am, err := state.getOrInitBranchAM(ctx, defaultBranch)
	require.NoError(t, err)
	primary, err := openCollection(ctx, state.cs, state.ns, must.NotFail(am.Get(ctx, "legacy")))
	require.NoError(t, err)
	oldLayout := must.NotFail(prolly.NewEmptyAddressMap(state.ns))
	dtblMsg := buildDoltTableFlatbuffer(primary, state.collSchemaHash, []byte(tree.ValueFromNode(oldLayout.Node()).(dolttypes.SerialMessage)), hash.Hash{})
	ref, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(dtblMsg))
	require.NoError(t, err)
	require.NoError(t, state.updateAddressMap(ctx, defaultBranch, "old layout", func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, "legacy", ref.TargetHash())
	}))
	state.mu.Unlock()
	be.Close()

	_, err = open()
	require.ErrorContains(t, err, "start with an empty data directory")
	require.ErrorContains(t, err, "admin.legacy")
}

// The definition is canonical: a partial filter or collation written in a
// different key order, at any depth, encodes to the same bytes and decodes
// with sorted keys.
func TestIndexDefinitionIsCanonical(t *testing.T) {
	define := func(partial, collation *types.Document) []byte {
		def, err := encodeIndexDefinition(backends.IndexInfo{
			Name: "p", Key: []backends.IndexKeyPair{{Field: "b"}, {Field: "a"}}, PartialFilterExpression: partial, Collation: collation,
		})
		require.NoError(t, err)
		return def
	}
	ba := define(
		must.NotFail(types.NewDocument("b", must.NotFail(types.NewDocument("y", int32(2), "x", int32(1))), "a", must.NotFail(types.NewDocument("$exists", true)))),
		must.NotFail(types.NewDocument("strength", int32(2), "locale", "en")),
	)
	ab := define(
		must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$exists", true)), "b", must.NotFail(types.NewDocument("x", int32(1), "y", int32(2))))),
		must.NotFail(types.NewDocument("locale", "en", "strength", int32(2))),
	)
	require.Equal(t, ab, ba)

	entry, err := resolveIndexEntry(ba, hash.Of([]byte("root")))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, entry.info.PartialFilterExpression.Keys())
	require.Equal(t, []string{"locale", "strength"}, entry.info.Collation.Keys())
	require.True(t, slices.Equal(entry.info.Key, []backends.IndexKeyPair{{Field: "b"}, {Field: "a"}}), "compound key order is kept")
	match, err := entry.info.MatchesPartialFilter(must.NotFail(types.NewDocument("a", int32(1), "b", must.NotFail(types.NewDocument("x", int32(1), "y", int32(2))))))
	require.NoError(t, err)
	require.True(t, match)
}

// A root field that is not a hash is an error, not a panic.
func TestSecondaryIndexValueRejectsShortRoot(t *testing.T) {
	ctx := context.Background()
	desc := val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc}, val.Type{Enc: val.ByteStringEnc})
	tb := val.NewTupleBuilder(desc, nil)
	tb.PutByteString(0, []byte{0})
	tb.PutByteString(1, []byte{1, 2, 3, 4, 5})
	tup, err := tb.Build(ctx, bufPool)
	require.NoError(t, err)
	_, _, err = readSecondaryIndexValue(ctx, tree.NewTestNodeStore(), tup)
	require.ErrorContains(t, err, "invalid index map root length")
}

// Partial filters compare in canonical form, so filters that differ only in
// key order, at any depth, are the same definition.
func TestIndexSpecEqualIgnoresPartialFilterKeyOrder(t *testing.T) {
	spec := func(partial *types.Document) backends.IndexInfo {
		return backends.IndexInfo{Name: "p", Key: []backends.IndexKeyPair{{Field: "a"}}, PartialFilterExpression: partial}
	}
	ab := must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$gt", int32(1))), "b", must.NotFail(types.NewDocument("$exists", true))))
	ba := must.NotFail(types.NewDocument("b", must.NotFail(types.NewDocument("$exists", true)), "a", must.NotFail(types.NewDocument("$gt", int32(1)))))
	xy := must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("x", int32(1), "y", int32(2)))))
	yx := must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("y", int32(2), "x", int32(1)))))
	other := must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$gt", int32(2))), "b", must.NotFail(types.NewDocument("$exists", true))))
	require.True(t, indexSpecEqual(spec(ab), spec(ba)))
	require.True(t, indexSpecEqual(spec(xy), spec(yx)))
	require.False(t, indexSpecEqual(spec(ab), spec(other)))
}

// Two branches that create the same index with the partial filter written in
// a different key order merge cleanly.
func TestMergeSameIndexWithReorderedPartialFilter(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mdb", "items", mustDoc(t, "_id", int32(1), "a", int32(5), "b", int32(1)))
	commitDB(t, b, "mdb", "base")
	branchFrom(t, b, "mdb", "main", "feat")
	create := func(branch string, partial *types.Document) {
		coll := collAt(t, b, "mdb", branch, "items")
		_, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{
			Name: "p", Key: []backends.IndexKeyPair{{Field: "a"}}, PartialFilterExpression: partial,
		}}})
		require.NoError(t, err)
		commitBranch(t, b, "mdb", branch, "create p on "+branch)
	}
	create("main", must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$gt", int32(1))), "b", must.NotFail(types.NewDocument("$exists", true)))))
	create("feat", must.NotFail(types.NewDocument("b", must.NotFail(types.NewDocument("$exists", true)), "a", must.NotFail(types.NewDocument("$gt", int32(1))))))
	mainDef := secondaryIndexEntry(t, ctx, secondaryIndexesOnDisk(t, ctx, b, "mdb", "main", "items"), "p")
	featDef := secondaryIndexEntry(t, ctx, secondaryIndexesOnDisk(t, ctx, b, "mdb", "feat", "items"), "p")
	require.Equal(t, definitionHash(t, mainDef.info), definitionHash(t, featDef.info), "reordered filters store the same definition")

	mergeBranches(t, b, "mdb", "main", "feat")
	require.Equal(t, []string{"_id_", "p"}, listIndexNames(t, ctx, collAt(t, b, "mdb", "main", "items")))
}

// Writes change index map roots but not definitions, so they do not grow
// the definition memo.
func TestIndexedWritesDoNotGrowDefinitionMemo(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	coll := must.NotFail(must.NotFail(b.Database("memodb")).Collection("coll"))
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(0), "k", int32(0)))
	createIndex(t, ctx, coll, "k_1", "k")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "k", int32(1)))
	before := indexDefinitionMemoLen()
	for i := int32(2); i <= 200; i++ {
		insertOne(t, ctx, coll, mustDoc(t, "_id", i, "k", i))
	}
	require.Equal(t, before, indexDefinitionMemoLen())
}
