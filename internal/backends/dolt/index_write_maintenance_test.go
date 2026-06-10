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

// Behaviors W2-W5, M1/M2 (stored-content halves), and P2 of
// docs/design/secondary-index-structural-sharing.md.

import (
	"context"
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func updateDoc(t *testing.T, ctx context.Context, coll backends.Collection, doc *types.Document) {
	t.Helper()
	if _, err := coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{doc},
	}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}
}

func deleteByID(t *testing.T, ctx context.Context, coll backends.Collection, id any) {
	t.Helper()
	if _, err := coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
}

func indexEntryCount(t *testing.T, ctx context.Context, b *Backend, dbName, branch, collName, idxName string) int {
	t.Helper()
	m := indexMapOnBranch(t, ctx, b, dbName, branch, collName, idxName)
	n, err := m.Count()
	if err != nil {
		t.Fatalf("index map Count: %v", err)
	}
	return n
}

func TestUpdateReindexesChangedField(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "alpha"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(2), "field", "bravo"))

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1), "field", "zulu"))

	if got := equalityLookupIDs(t, ctx, coll, "field", "zulu"); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("lookup by new value = %v, want [1]", got)
	}
	if got := equalityLookupIDs(t, ctx, coll, "field", "alpha"); len(got) != 0 {
		t.Errorf("lookup by old value = %v, want empty", got)
	}
	if got := indexedCount(t, ctx, coll, "field", "alpha"); got != 0 {
		t.Errorf("indexed count of old value = %d, want 0", got)
	}
}

func TestDeleteRemovesIndexEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "alpha"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(2), "field", "alpha"))

	deleteByID(t, ctx, coll, int32(1))

	if got := equalityLookupIDs(t, ctx, coll, "field", "alpha"); !reflect.DeepEqual(got, []int32{2}) {
		t.Errorf("lookup after delete = %v, want [2]", got)
	}
	if got := indexedCount(t, ctx, coll, "field", "alpha"); got != 1 {
		t.Errorf("indexed count after delete = %d, want 1", got)
	}
}

// W4: an update that does not change any indexed field leaves the
// index root hash unchanged.
func TestNoopUpdateLeavesIndexRootUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items",
		mustDoc(t, "_id", int32(1), "field", "alpha", "other", int32(1)))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")

	before := indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field").HashOf()

	updateDoc(t, ctx, coll,
		mustDoc(t, "_id", int32(1), "field", "alpha", "other", int32(2)))

	after := indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field").HashOf()
	if before != after {
		t.Errorf("index root changed on a noop update: %s -> %s", before, after)
	}
}

// W5: an array update that keeps most elements edits only the changed
// entries.
func TestMultikeyUpdateAdjustsOnlyChangedElements(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mkArr := func(vals ...any) *types.Array {
		a := types.MakeArray(len(vals))
		for _, v := range vals {
			a.Append(v)
		}
		return a
	}

	insertDoc(t, b, "testdb", "items",
		mustDoc(t, "_id", int32(1), "tags", mkArr("red", "green", "blue")))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_tags", "tags")

	updateDoc(t, ctx, coll,
		mustDoc(t, "_id", int32(1), "tags", mkArr("red", "yellow", "blue")))

	for _, c := range []struct {
		value string
		want  int
	}{
		{"red", 1}, {"blue", 1}, {"yellow", 1}, {"green", 0},
	} {
		if got := equalityLookupIDs(t, ctx, coll, "tags", c.value); len(got) != c.want {
			t.Errorf("lookup %q = %v, want %d hit(s)", c.value, got, c.want)
		}
	}
	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_tags"); got != 3 {
		t.Errorf("index entry count = %d, want 3", got)
	}
}

// M1: a sparse index holds entries only for docs that have the field.
func TestSparseIndexMembershipContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "alpha"))
	coll := collAt(t, b, "testdb", "main", "items")
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:   "by_field_sparse",
			Key:    []backends.IndexKeyPair{{Field: "field"}},
			Sparse: true,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(2), "other", int32(7)))

	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_sparse"); got != 1 {
		t.Errorf("sparse index entry count = %d, want 1 (missing-field doc must not be indexed)", got)
	}

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(2), "field", "bravo"))
	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_sparse"); got != 2 {
		t.Errorf("sparse index entry count after field add = %d, want 2", got)
	}

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(2), "other", int32(8)))
	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_sparse"); got != 1 {
		t.Errorf("sparse index entry count after field unset = %d, want 1", got)
	}
}

// M2: a partial index holds entries only while the doc satisfies the
// filter, across boundary crossings in both directions.
func TestPartialIndexMembershipContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items",
		mustDoc(t, "_id", int32(1), "field", "alpha", "status", "active"))
	coll := collAt(t, b, "testdb", "main", "items")

	pf := must.NotFail(types.NewDocument("status", "active"))
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:                    "by_field_partial",
			Key:                     []backends.IndexKeyPair{{Field: "field"}},
			PartialFilterExpression: pf,
			MatchesPartialFilter: func(doc *types.Document) (bool, error) {
				v, err := doc.Get("status")
				return err == nil && v == "active", nil
			},
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	insertOne(t, ctx, coll,
		mustDoc(t, "_id", int32(2), "field", "bravo", "status", "inactive"))

	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_partial"); got != 1 {
		t.Errorf("partial index entry count = %d, want 1 (non-member must not be indexed)", got)
	}

	updateDoc(t, ctx, coll,
		mustDoc(t, "_id", int32(1), "field", "alpha", "status", "inactive"))
	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_partial"); got != 0 {
		t.Errorf("partial index entry count after member left = %d, want 0", got)
	}

	updateDoc(t, ctx, coll,
		mustDoc(t, "_id", int32(2), "field", "bravo", "status", "active"))
	if got := indexEntryCount(t, ctx, b, "testdb", "main", "items", "by_field_partial"); got != 1 {
		t.Errorf("partial index entry count after non-member joined = %d, want 1", got)
	}
}

// P2: chunks outside the touched key range keep their addresses across
// update and delete.
func TestIndexChunkReuseAcrossWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "seed"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")

	bulkInsert(t, ctx, coll, 1000, 3000, "v")

	before := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field"))
	if len(before) < 3 {
		t.Fatalf("index tree too small to measure sharing (%d chunks)", len(before))
	}

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1000), "field", "zzz-moved"))
	afterUpdate := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field"))
	if shared := countSharedChunks(before, afterUpdate); shared == 0 {
		t.Errorf("update rewrote the whole index tree; expected chunk reuse (before=%d after=%d shared=0)",
			len(before), len(afterUpdate))
	}

	deleteByID(t, ctx, coll, int32(1001))
	afterDelete := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field"))
	if shared := countSharedChunks(afterUpdate, afterDelete); shared == 0 {
		t.Errorf("delete rewrote the whole index tree; expected chunk reuse")
	}
}
