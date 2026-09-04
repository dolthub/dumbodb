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

// Test helpers for branch-scoped index behaviour. See
// docs/design/branch-scoped-index-metadata.md section 5.1.

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"testing"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// openBackendInDir constructs a Backend rooted at dir without registering a
// cleanup that removes the directory. Use when a test needs to close and
// reopen the same on-disk state across two Backend instances.
func openBackendInDir(t *testing.T, dir string) *Backend {
	t.Helper()
	return &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
}

// branchFrom creates a new branch named "name" off "from" in dbName.
func branchFrom(t *testing.T, b *Backend, dbName, from, name string) {
	t.Helper()
	if _, err := b.DumboDBBranch(context.Background(), &backends.BranchParams{
		Action: "add",
		DBName: dbName,
		From:   from,
		Name:   name,
	}); err != nil {
		t.Fatalf("DumboDBBranch(%s from %s): %v", name, from, err)
	}
}

// commitBranch commits the current working set of (dbName, branch).
func commitBranch(t *testing.T, b *Backend, dbName, branch, message string) string {
	t.Helper()
	res, err := b.DumboDBCommit(context.Background(), &backends.CommitParams{
		DBName:  dbName,
		Branch:  branch,
		Message: message,
		Author:  "testuser",
	})
	if err != nil {
		t.Fatalf("DumboDBCommit(%s@%s, %q): %v", dbName, branch, message, err)
	}
	return res.CommitID
}

// collAt returns a Collection handle for (dbName, branch, collName). The
// returned handle is pinned to the given branch via the "@" suffix on the
// database name; subsequent reads and writes use that branch as their
// rootish.
func collAt(t *testing.T, b *Backend, dbName, branch, collName string) backends.Collection {
	t.Helper()
	qualified := dbName + "@" + branch
	if branch == defaultBranch {
		qualified = dbName
	}
	db, err := b.Database(qualified)
	if err != nil {
		t.Fatalf("Database(%q): %v", qualified, err)
	}
	coll, err := db.Collection(collName)
	if err != nil {
		t.Fatalf("Collection(%q): %v", collName, err)
	}
	return coll
}

// createIndex creates a single named secondary index on the collection over
// one field.
func createIndex(t *testing.T, ctx context.Context, coll backends.Collection, name, field string) {
	t.Helper()
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: name, Key: []backends.IndexKeyPair{{Field: field}}},
		},
	}); err != nil {
		t.Fatalf("CreateIndexes(%q): %v", name, err)
	}
}

// listIndexNames returns the sorted slice of index names on the collection.
func listIndexNames(t *testing.T, ctx context.Context, coll backends.Collection) []string {
	t.Helper()
	res, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	names := make([]string, 0, len(res.Indexes))
	for _, idx := range res.Indexes {
		names = append(names, idx.Name)
	}
	sort.Strings(names)
	return names
}

// insertOne inserts a single document.
func insertOne(t *testing.T, ctx context.Context, coll backends.Collection, doc *types.Document) {
	t.Helper()
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}
}

// equalityLookupIDs runs an equality filter through Query and returns the
// sorted set of matched _id values as int32. Drives the same tryIndexLookup
// path the production planner uses; the handler-side re-filter is not
// applied here, so the result is the index path's view of "matches".
func equalityLookupIDs(t *testing.T, ctx context.Context, coll backends.Collection, field string, value any) []int32 {
	t.Helper()
	filter := must.NotFail(types.NewDocument(field, value))
	docs := drainQuery(t, ctx, coll, &backends.QueryParams{Filter: filter})
	ids := idsInDocs(t, docs)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// indexedCount returns the count produced by tryIndexedCount via
// coll.Count with a single-field equality filter. Unlike equalityLookupIDs,
// this does NOT consult the primary -- it returns the count of index
// entries directly. That makes it the right probe for "is the index
// itself correctly partitioned per branch?" because primary-fetch
// disambiguation cannot mask a polluted index.
func indexedCount(t *testing.T, ctx context.Context, coll backends.Collection, field string, value any) int64 {
	t.Helper()
	filter := must.NotFail(types.NewDocument(field, value))
	res, err := coll.Count(ctx, &backends.CountParams{Filter: filter})
	if err != nil {
		t.Fatalf("Count(filter=%v): %v", filter, err)
	}
	if !res.Filtered {
		t.Fatalf("Count did not use the index (Filtered=false); test premise broken")
	}
	return res.Count
}

// indexAMOnDisk reads the per-collection secondary_indexes AddressMap
// from the branch's DTBL chunk, bypassing all in-memory dbState caches.
// This is the authoritative "what does the on-disk DTBL say" probe and
// is the only way TestWrite_OnFeat_DoesNotCorruptMainDTBL can detect the
// cross-branch corruption bug.
func indexAMOnDisk(t *testing.T, ctx context.Context, b *Backend, dbName, branch, collName string) prolly.AddressMap {
	t.Helper()

	state, err := b.getOrOpenDB(ctx, dbName, false)
	if err != nil {
		t.Fatalf("getOrOpenDB(%q): %v", dbName, err)
	}
	if state == nil {
		t.Fatalf("getOrOpenDB(%q): nil state", dbName)
	}

	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+branch))
	if err != nil {
		t.Fatalf("ResolveWorkingSet(%s): %v", branch, err)
	}

	collAM, err := amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
	if err != nil {
		t.Fatalf("amFromWorkingRoot: %v", err)
	}

	dtblHash, err := collAM.Get(ctx, collName)
	if err != nil {
		t.Fatalf("collAM.Get(%q): %v", collName, err)
	}
	if dtblHash.IsEmpty() {
		t.Fatalf("collection %q has no DTBL on branch %q", collName, branch)
	}

	chunk, err := state.cs.Get(ctx, dtblHash)
	if err != nil {
		t.Fatalf("cs.Get DTBL: %v", err)
	}
	if serial.GetFileID(chunk.Data()) != serial.TableFileID {
		t.Fatalf("DTBL chunk has unexpected file id")
	}

	tbl, err := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		t.Fatalf("parsing DTBL: %v", err)
	}

	idxBytes := tbl.SecondaryIndexesBytes()
	if len(idxBytes) == 0 {
		empty, eerr := prolly.NewEmptyAddressMap(state.ns)
		if eerr != nil {
			t.Fatalf("NewEmptyAddressMap: %v", eerr)
		}
		return empty
	}

	amNode, _, err := tree.NodeFromBytes(idxBytes)
	if err != nil {
		t.Fatalf("NodeFromBytes: %v", err)
	}
	idxAM, err := prolly.NewAddressMap(amNode, state.ns)
	if err != nil {
		t.Fatalf("NewAddressMap: %v", err)
	}
	return idxAM
}

// indexNamesInAM returns the sorted slice of names in an index AddressMap.
func indexNamesInAM(t *testing.T, ctx context.Context, am prolly.AddressMap) []string {
	t.Helper()
	var names []string
	err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
		names = append(names, name)
		return nil
	})
	if err != nil {
		t.Fatalf("am.IterAll: %v", err)
	}
	sort.Strings(names)
	return names
}
