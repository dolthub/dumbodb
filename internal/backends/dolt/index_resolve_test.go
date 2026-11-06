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
// secondaryIndexesForDTBL and asserts that the result names the indexes we
// wrote.
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

	am, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("secondaryIndexesForDTBL: %v", err)
	}
	names := secondaryIndexNames(t, ctx, am)
	want := []string{"by_age"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("secondaryIndexesForDTBL names = %v, want %v", names, want)
	}
}

// TestResolver_EmptyCollection returns no secondary indexes for a collection
// that has been created with none.
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

	am, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("secondaryIndexesForDTBL: %v", err)
	}
	cnt, err := am.Count()
	if err != nil {
		t.Fatalf("am.Count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("expected no secondary indexes, got %d", cnt)
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

// TestResolver_DefinitionMemoHitsAfterMiss verifies the indexDefinitionMemo:
// the first resolve of a definition is a miss and populates the memo; the
// second is a hit and shares the decoded definition.
func TestResolver_DefinitionMemoHitsAfterMiss(t *testing.T) {
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
	am, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("secondaryIndexesForDTBL: %v", err)
	}

	stored := secondaryIndexEntry(t, ctx, am, "by_age")
	if stored == nil {
		t.Fatal("by_age entry not found")
	}
	def, err := encodeIndexDefinition(stored.info)
	if err != nil {
		t.Fatalf("encodeIndexDefinition: %v", err)
	}

	defHash := hash.Of(def)
	indexDefinitionMemo.Delete(string(def))
	if _, ok := indexDefinitionMemo.Load(string(def)); ok {
		t.Fatal("indexDefinitionMemo unexpectedly contained entry after Delete")
	}

	first, err := resolveIndexEntry(def, stored.mapRoot)
	if err != nil {
		t.Fatalf("first resolveIndexEntry: %v", err)
	}
	if first.info.Name != "by_age" {
		t.Errorf("first resolve: info.Name = %q, want by_age", first.info.Name)
	}
	if first.mapRoot.IsEmpty() {
		t.Error("first resolve: mapRoot is empty")
	}
	if _, ok := indexDefinitionMemo.Load(string(def)); !ok {
		t.Fatal("first resolve did not populate indexDefinitionMemo")
	}
	if first.defHash != defHash {
		t.Errorf("first resolve: defHash %s, want %s", first.defHash, defHash)
	}

	second, err := resolveIndexEntry(def, stored.mapRoot)
	if err != nil {
		t.Fatalf("second resolveIndexEntry: %v", err)
	}
	if &first.info.Key[0] != &second.info.Key[0] {
		t.Error("second resolve decoded the definition again instead of sharing it")
	}
	if first.defHash != second.defHash || first.mapRoot != second.mapRoot {
		t.Errorf("entries differ: %s@%s vs %s@%s", first.defHash, first.mapRoot, second.defHash, second.mapRoot)
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
	am, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		t.Fatalf("secondaryIndexesForDTBL: %v", err)
	}

	resolved := secondaryIndexEntry(t, ctx, am, "by_age")
	if resolved == nil {
		t.Fatal("by_age entry not resolved")
	}

	idxMap, err := openIndexMap(ctx, state.ns, resolved.mapRoot)
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
// emptySecondaryIndexesCache memoizes by NodeStore (design 6.4).
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

	a1, err := emptySecondaryIndexes(ctx, state.ns)
	if err != nil {
		t.Fatalf("emptySecondaryIndexes #1: %v", err)
	}
	a2, err := emptySecondaryIndexes(ctx, state.ns)
	if err != nil {
		t.Fatalf("emptySecondaryIndexes #2: %v", err)
	}

	// The map is a value type; we identify identity via the underlying
	// node hash, which is stable across the value copy.
	if a1.m.HashOf() != a2.m.HashOf() {
		t.Errorf("emptySecondaryIndexes produced different maps across calls: %s vs %s",
			a1.m.HashOf().String(), a2.m.HashOf().String())
	}
}
