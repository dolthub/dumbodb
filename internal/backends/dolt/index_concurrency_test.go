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

// Concurrency and memo-behaviour tests for the branch-scoped index
// resolver. See docs/design/branch-scoped-index-metadata.md sections
// 5.5 and 5.6.

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// indexEntryMemoLen counts entries in the process-wide indexEntryMemo.
// sync.Map has no Len; we walk it. Cheap for test sizes.
func indexEntryMemoLen() int {
	n := 0
	indexEntryMemo.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// indexEntryMemoKeys returns the set of hash.Hash keys currently in the
// memo. Useful for cross-branch dedup assertions.
func indexEntryMemoKeys() map[hash.Hash]struct{} {
	out := make(map[hash.Hash]struct{})
	indexEntryMemo.Range(func(k, _ any) bool {
		out[k.(hash.Hash)] = struct{}{}
		return true
	})
	return out
}

// TestMemo_DedupesAcrossBranches verifies that reading the same index
// from two branches resolves to the same memo entry. Branches that
// share an index by hash should share one decoded IndexInfo.
func TestMemo_DedupesAcrossBranches(t *testing.T) {
	// Cannot t.Parallel: inspects global memo state.

	ctx := context.Background()
	b := newTestBackend(t)

	// Seed main and create an index. branch feat off main, do nothing
	// else so feat's DTBL identifies the same IndexEntry hash as main.
	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30)))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "main: seed + by_age")

	branchFrom(t, b, "testdb", "main", "feat")

	// Snapshot the memo size after the index was created (writes touch
	// the memo too, indirectly, through the resolver path used by
	// CreateIndexes). We measure the deduplication on the *read* side
	// by checking the memo grew by zero after the second branch's read.
	before := indexEntryMemoLen()

	mainNames := listIndexNames(t, ctx, mainColl)
	if got := []string{"_id_", "by_age"}; !reflect.DeepEqual(mainNames, got) {
		t.Fatalf("main listIndexes = %v, want %v", mainNames, got)
	}
	afterMain := indexEntryMemoLen()

	featNames := listIndexNames(t, ctx, collAt(t, b, "testdb", "feat", "items"))
	if got := []string{"_id_", "by_age"}; !reflect.DeepEqual(featNames, got) {
		t.Fatalf("feat listIndexes = %v, want %v", featNames, got)
	}
	afterFeat := indexEntryMemoLen()

	// main and feat point at the SAME IndexEntry chunk, so reading from
	// feat should not increase the memo size.
	if afterFeat != afterMain {
		t.Errorf("memo size after feat read = %d, after main read = %d; "+
			"expected the same hash to dedupe", afterFeat, afterMain)
	}

	// And the memo grew by at most one across both reads (one entry per
	// distinct IndexEntry hash). It may have grown by zero if the entry
	// was already populated by the write path.
	if delta := afterFeat - before; delta < 0 || delta > 1 {
		t.Errorf("memo growth across two reads = %d, want 0 or 1", delta)
	}
}

// TestMemo_DistinctEntriesForDistinctDefinitions verifies that two
// branches with different index definitions for the same name produce
// two distinct memo entries (content-addressed: different chunk bytes
// -> different hashes).
func TestMemo_DistinctEntriesForDistinctDefinitions(t *testing.T) {
	// Cannot t.Parallel: inspects global memo state.

	ctx := context.Background()
	b := newTestBackend(t)

	// Use a unique collection name to avoid collision with other tests
	// that may have populated the memo.
	collName := "mx_distinct_defs"
	mainItems := collAt(t, b, "testdb", "main", collName)
	insertOne(t, ctx, mainItems, mustDoc(t, "_id", int32(1), "x", int32(5)))
	createIndex(t, ctx, mainItems, "by_x", "x")
	commitDB(t, b, "testdb", "main: by_x on x")

	branchFrom(t, b, "testdb", "main", "feat")

	// Drop and recreate by_x on feat with a different field. Index names
	// are immutable per (per docs/design/secondary-index-structural-
	// sharing.md), so this is in spirit a different index sharing a
	// name -- which is the only way to get same-name-different-spec on
	// two branches.
	featItems := collAt(t, b, "testdb", "feat", collName)
	if _, err := featItems.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_x"}}); err != nil {
		t.Fatalf("DropIndexes by_x: %v", err)
	}
	createIndex(t, ctx, featItems, "by_x", "y")
	commitBranch(t, b, "testdb", "feat", "feat: by_x on y")

	// Read each branch's persisted IndexEntry chunk hash directly.
	mainAM := indexAMOnDisk(t, ctx, b, "testdb", "main", collName)
	mainEntry, err := mainAM.Get(ctx, "by_x")
	if err != nil {
		t.Fatalf("main AM.Get(by_x): %v", err)
	}
	featAM := indexAMOnDisk(t, ctx, b, "testdb", "feat", collName)
	featEntry, err := featAM.Get(ctx, "by_x")
	if err != nil {
		t.Fatalf("feat AM.Get(by_x): %v", err)
	}

	if mainEntry == featEntry {
		t.Fatalf("expected different IndexEntry hashes for by_x{x} vs by_x{y}; got equal %s", mainEntry.String())
	}

	// Resolve both. Both should be present in the memo with distinct keys.
	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	if _, err := resolveIndexEntry(ctx, state.ns, mainEntry); err != nil {
		t.Fatalf("resolveIndexEntry main: %v", err)
	}
	if _, err := resolveIndexEntry(ctx, state.ns, featEntry); err != nil {
		t.Fatalf("resolveIndexEntry feat: %v", err)
	}

	keys := indexEntryMemoKeys()
	if _, ok := keys[mainEntry]; !ok {
		t.Errorf("memo missing main's by_x entry %s", mainEntry.String())
	}
	if _, ok := keys[featEntry]; !ok {
		t.Errorf("memo missing feat's by_x entry %s", featEntry.String())
	}
}

// TestConcurrent_WritesOnDifferentBranches_DoNotBlock asserts that two
// goroutines writing to different branches both complete and produce
// per-branch-correct on-disk state. The shared dbState.mu still
// serialises (writes to different branches contend on it today; that's
// a workspace-i0u follow-up), but per-branch correctness must hold
// regardless of ordering.
func TestConcurrent_WritesOnDifferentBranches_DoNotBlock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "main: seed by_age")

	branchFrom(t, b, "testdb", "main", "a")
	branchFrom(t, b, "testdb", "main", "b")

	const perBranch = 50
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		coll := collAt(t, b, "testdb", "a", "items")
		for i := 0; i < perBranch; i++ {
			if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
				mustDoc(t, "_id", int32(1000+i), "age", int32(100+i)),
			}}); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		coll := collAt(t, b, "testdb", "b", "items")
		for i := 0; i < perBranch; i++ {
			if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
				mustDoc(t, "_id", int32(2000+i), "age", int32(200+i)),
			}}); err != nil {
				errCh <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent insert: %v", err)
		}
	}

	// Each branch sees its own data; neither sees the other's.
	aColl := collAt(t, b, "testdb", "a", "items")
	bColl := collAt(t, b, "testdb", "b", "items")
	for i := 0; i < perBranch; i++ {
		if got := equalityLookupIDs(t, ctx, aColl, "age", int32(100+i)); !equalInt32Slices(got, []int32{int32(1000 + i)}) {
			t.Errorf("a: lookup age=%d -> %v, want [%d]", 100+i, got, 1000+i)
		}
		if got := equalityLookupIDs(t, ctx, aColl, "age", int32(200+i)); len(got) != 0 {
			t.Errorf("a: lookup age=%d (b's range) -> %v, want []", 200+i, got)
		}
		if got := equalityLookupIDs(t, ctx, bColl, "age", int32(200+i)); !equalInt32Slices(got, []int32{int32(2000 + i)}) {
			t.Errorf("b: lookup age=%d -> %v, want [%d]", 200+i, got, 2000+i)
		}
		if got := equalityLookupIDs(t, ctx, bColl, "age", int32(100+i)); len(got) != 0 {
			t.Errorf("b: lookup age=%d (a's range) -> %v, want []", 100+i, got)
		}
	}
}

// TestConcurrent_ReadOnA_WriteOnB asserts that a continuous read stream
// on branch a is stable while branch b is being written to. None of
// b's writes leak into a's view through the index.
func TestConcurrent_ReadOnA_WriteOnB(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	aColl := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, aColl, "by_age", "age")
	for i := 0; i < 20; i++ {
		insertOne(t, ctx, aColl, mustDoc(t, "_id", int32(1+i), "age", int32(30+i)))
	}
	commitDB(t, b, "testdb", "seed on main")

	branchFrom(t, b, "testdb", "main", "b")

	// Baseline: what a (which IS main) sees for each value.
	baseline := make(map[int32][]int32, 20)
	for i := 0; i < 20; i++ {
		baseline[int32(30+i)] = equalityLookupIDs(t, ctx, aColl, "age", int32(30+i))
	}

	const writes = 200
	const reads = 1000

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer on b.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bColl := collAt(t, b, "testdb", "b", "items")
		for i := 0; i < writes; i++ {
			if _, err := bColl.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
				mustDoc(t, "_id", int32(5000+i), "age", int32(500+i)),
			}}); err != nil {
				t.Errorf("b insert %d: %v", i, err)
				return
			}
		}
		close(stop)
	}()

	// Reader on a (main). Iterates over baseline values, asserts they
	// match. Polls in a tight loop until the writer is done.
	wg.Add(1)
	go func() {
		defer wg.Done()
		readCount := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			for age, wantIDs := range baseline {
				got := equalityLookupIDs(t, ctx, aColl, "age", age)
				if !equalInt32Slices(got, wantIDs) {
					t.Errorf("a: lookup age=%d during concurrent writes = %v, want %v", age, got, wantIDs)
					return
				}
				readCount++
				if readCount >= reads {
					return
				}
			}
		}
	}()

	wg.Wait()
}

// TestReopen_NoEagerHydration verifies that opening a database with
// indexed collections does NOT walk every DTBL at open. The eager
// hydrate path is deleted as of workspace-5iq; the memo should be
// empty for indexes that nobody has read yet.
func TestReopen_NoEagerHydration(t *testing.T) {
	// Cannot t.Parallel: inspects global memo state.

	ctx := context.Background()
	dir := t.TempDir()

	// Phase 1: create the data, then close the backend.
	{
		bk := openBackendInDir(t, dir)
		mainColl := collAt(t, bk, "testdb", "main", "items")
		insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30)))
		createIndex(t, ctx, mainColl, "by_age", "age")
		commitDB(t, bk, "testdb", "seed + by_age")

		branchFrom(t, bk, "testdb", "main", "feat")
		featColl := collAt(t, bk, "testdb", "feat", "items")
		createIndex(t, ctx, featColl, "by_id_age", "age")
		commitBranch(t, bk, "testdb", "feat", "feat: by_id_age")

		bk.Close()
	}

	// Snapshot the memo, then reopen the backend. Force the memo to
	// capture only changes from this reopen by recording keys before
	// and after.
	keysBefore := indexEntryMemoKeys()

	bk2 := openBackendInDir(t, dir)
	t.Cleanup(bk2.Close)

	// Force getOrOpenDB to run by opening a database handle. This is
	// where hydrateAllIndexes used to fire.
	if _, err := bk2.Database("testdb"); err != nil {
		t.Fatalf("Database: %v", err)
	}
	// Also call getOrOpenDB directly to force db-state initialisation.
	if _, err := bk2.getOrOpenDB(ctx, "testdb", false); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	keysAfterOpen := indexEntryMemoKeys()
	if !sameKeySet(keysBefore, keysAfterOpen) {
		t.Errorf("memo populated at db-open without any read; got new keys: %v",
			diffKeys(keysAfterOpen, keysBefore))
	}

	// First read on feat resolves and populates the memo.
	// feat inherited by_age from main and added by_id_age.
	names := listIndexNames(t, ctx, collAt(t, bk2, "testdb", "feat", "items"))
	if !reflect.DeepEqual(names, []string{"_id_", "by_age", "by_id_age"}) {
		t.Errorf("feat listIndexes after reopen = %v, want [_id_ by_age by_id_age]", names)
	}

	keysAfterRead := indexEntryMemoKeys()
	if len(keysAfterRead) <= len(keysAfterOpen) {
		t.Errorf("memo did not grow after first read: before=%d after=%d",
			len(keysAfterOpen), len(keysAfterRead))
	}
}

// sameKeySet reports whether two key sets are equal.
func sameKeySet(a, b map[hash.Hash]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// diffKeys returns the keys in a that are not in b, as a sorted slice
// of hex strings.
func diffKeys(a, b map[hash.Hash]struct{}) []string {
	out := make([]string, 0)
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k.String())
		}
	}
	sort.Strings(out)
	return out
}
