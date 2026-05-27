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

// Unit tests for the resolver functions in index_resolve.go. No call site
// in production uses them yet -- the tests exercise the resolver chain
// directly against a backend populated by ordinary CreateIndexes /
// InsertAll writes.

import (
	"context"
	"reflect"
	"testing"

	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"
)

// TestResolver_RoundTripIndexAMFromDTBL exercises dtblHashForColl ->
// indexAMForDTBL and asserts that the returned AddressMap names the
// indexes we wrote.
func TestResolver_RoundTripIndexAMFromDTBL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "seed + by_age")

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/main"))
	if err != nil {
		t.Fatalf("ResolveWorkingSet: %v", err)
	}

	dtblHash, err := dtblHashForColl(ctx, state.ns, ws.WorkingRoot(), "items")
	if err != nil {
		t.Fatalf("dtblHashForColl: %v", err)
	}
	if dtblHash.IsEmpty() {
		t.Fatal("dtblHashForColl returned empty hash for an existing collection")
	}

	am, err := indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("indexAMForDTBL: %v", err)
	}
	names := indexNamesInAM(t, ctx, am)
	want := []string{"by_age"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("indexAMForDTBL names = %v, want %v", names, want)
	}
}

// TestResolver_EmptyCollection returns the empty AM for a collection that
// has been created with no secondary indexes.
func TestResolver_EmptyCollection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "empty", mustDoc(t, "_id", int32(1)))
	commitDB(t, b, "testdb", "seed empty coll")

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/main"))
	if err != nil {
		t.Fatalf("ResolveWorkingSet: %v", err)
	}

	dtblHash, err := dtblHashForColl(ctx, state.ns, ws.WorkingRoot(), "empty")
	if err != nil {
		t.Fatalf("dtblHashForColl: %v", err)
	}

	am, err := indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("indexAMForDTBL: %v", err)
	}
	cnt, err := am.Count()
	if err != nil {
		t.Fatalf("am.Count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("expected empty index AM, got %d entries", cnt)
	}
}

// TestResolver_MissingCollection returns the zero hash for a collection
// that does not exist in the working root.
func TestResolver_MissingCollection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize the db with at least one commit so the working root is set.
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1)))
	commitDB(t, b, "testdb", "seed")

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/main"))
	if err != nil {
		t.Fatalf("ResolveWorkingSet: %v", err)
	}

	h, err := dtblHashForColl(ctx, state.ns, ws.WorkingRoot(), "no_such_coll")
	if err != nil {
		t.Fatalf("dtblHashForColl: %v", err)
	}
	if !h.IsEmpty() {
		t.Errorf("expected empty hash for missing collection, got %s", h.String())
	}
}

// TestResolver_IndexEntryMemoHitsAfterMiss verifies the indexEntryMemo:
// the first resolve for an entry hash is a miss and populates the memo;
// the second is a hit and returns the same pointer.
func TestResolver_IndexEntryMemoHitsAfterMiss(t *testing.T) {
	// Cannot t.Parallel: the test inspects global memo state.

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30)))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "seed + by_age")

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/main"))
	if err != nil {
		t.Fatalf("ResolveWorkingSet: %v", err)
	}
	dtblHash, err := dtblHashForColl(ctx, state.ns, ws.WorkingRoot(), "items")
	if err != nil {
		t.Fatalf("dtblHashForColl: %v", err)
	}
	am, err := indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("indexAMForDTBL: %v", err)
	}

	var entryHash hash.Hash
	if err := am.IterAll(ctx, func(name string, h hash.Hash) error {
		if name == "by_age" {
			entryHash = h
		}
		return nil
	}); err != nil {
		t.Fatalf("am.IterAll: %v", err)
	}
	if entryHash.IsEmpty() {
		t.Fatal("by_age IndexEntry not found in AM")
	}

	indexEntryMemo.Delete(entryHash)
	if _, ok := indexEntryMemo.Load(entryHash); ok {
		t.Fatal("indexEntryMemo unexpectedly contained entry after Delete")
	}

	first, err := resolveIndexEntry(ctx, state.ns, entryHash)
	if err != nil {
		t.Fatalf("first resolveIndexEntry: %v", err)
	}
	if first.info.Name != "by_age" {
		t.Errorf("first resolve: info.Name = %q, want by_age", first.info.Name)
	}
	if first.mapRoot.IsEmpty() {
		t.Error("first resolve: mapRoot is empty")
	}

	second, err := resolveIndexEntry(ctx, state.ns, entryHash)
	if err != nil {
		t.Fatalf("second resolveIndexEntry: %v", err)
	}
	if first != second {
		t.Errorf("memo did not return same pointer: %p vs %p", first, second)
	}
}

// TestResolver_OpenIndexMap reads through the full chain to the
// prolly.Map handle and confirms a known index entry is present.
func TestResolver_OpenIndexMap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30)))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "seed + by_age")

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/main"))
	if err != nil {
		t.Fatalf("ResolveWorkingSet: %v", err)
	}
	dtblHash, err := dtblHashForColl(ctx, state.ns, ws.WorkingRoot(), "items")
	if err != nil {
		t.Fatalf("dtblHashForColl: %v", err)
	}
	am, err := indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("indexAMForDTBL: %v", err)
	}

	var resolved *resolvedIndexEntry
	if err := am.IterAll(ctx, func(name string, h hash.Hash) error {
		if name != "by_age" {
			return nil
		}
		r, rerr := resolveIndexEntry(ctx, state.ns, h)
		if rerr != nil {
			return rerr
		}
		resolved = r
		return nil
	}); err != nil {
		t.Fatalf("am.IterAll: %v", err)
	}
	if resolved == nil {
		t.Fatal("by_age IndexEntry not resolved")
	}

	idxMap, err := openIndexMap(ctx, state.vs, state.ns, resolved.mapRoot)
	if err != nil {
		t.Fatalf("openIndexMap: %v", err)
	}
	cnt, err := idxMap.Count()
	if err != nil {
		t.Fatalf("idxMap.Count: %v", err)
	}
	if cnt != 1 {
		t.Errorf("by_age index Count = %d, want 1", cnt)
	}
}

// TestResolver_EmptyAMSharedAcrossCallsForSameNodeStore confirms the
// emptyIndexAMCache memoizes by NodeStore (design 6.4).
func TestResolver_EmptyAMSharedAcrossCallsForSameNodeStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)
	// Force a state so we have a NodeStore.
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1)))

	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	a1, err := emptyIndexAM(state.ns)
	if err != nil {
		t.Fatalf("emptyIndexAM #1: %v", err)
	}
	a2, err := emptyIndexAM(state.ns)
	if err != nil {
		t.Fatalf("emptyIndexAM #2: %v", err)
	}

	// AddressMap is a value type; we identify identity via the underlying
	// node hash, which is stable across the value copy.
	if a1.HashOf() != a2.HashOf() {
		t.Errorf("emptyIndexAM produced different AMs across calls: %s vs %s",
			a1.HashOf().String(), a2.HashOf().String())
	}
}
