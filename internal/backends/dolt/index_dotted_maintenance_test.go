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
	"bytes"
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Queries never read dotted-path indexes today (the planner skips dotted
// filter fields), so these tests read the index maps directly.

// rawCollection returns the unwrapped collection for dbName (optionally
// "db@branch") so tests can read its index maps.
func rawCollection(b *Backend, dbName, collName string) *collection {
	base, rootish := splitEncodedDBName(dbName)
	return &collection{db: &database{backend: b, name: base, rootish: rootish}, name: collName}
}

func resolvedIndex(t *testing.T, ctx context.Context, c *collection, name string) (backends.IndexInfo, func(value any) [][]byte) {
	t.Helper()
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	state.mu.RLock()
	infos, maps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		t.Fatalf("resolveIndexes: %v", err)
	}
	for i, info := range infos {
		if info.Name != name {
			continue
		}
		lookup := func(value any) [][]byte {
			ids, err := idxpkg.EqualityLookup(ctx, maps[i], value)
			if err != nil {
				t.Fatalf("EqualityLookup(%v): %v", value, err)
			}
			return ids
		}
		return info, lookup
	}
	t.Fatalf("index %q not found", name)
	return backends.IndexInfo{}, nil
}

// indexHolders returns which of candidates the index lists under value.
func indexHolders(t *testing.T, ctx context.Context, coll *collection, name string, value any, candidates ...int32) []int32 {
	t.Helper()
	_, lookup := resolvedIndex(t, ctx, coll, name)
	held := lookup(value)
	out := []int32{}
	for _, id := range candidates {
		h := must.NotFail(hashID(id))
		for _, got := range held {
			if bytes.Equal(got, h[:]) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func expectHolders(t *testing.T, ctx context.Context, coll *collection, name string, value any, want []int32, candidates ...int32) {
	t.Helper()
	if got := indexHolders(t, ctx, coll, name, value, candidates...); !reflect.DeepEqual(got, want) {
		t.Fatalf("index %s holds %v under %v, want %v", name, got, value, want)
	}
}

func abDoc(id int32, a any) *types.Document {
	return must.NotFail(types.NewDocument("_id", id, "a", a))
}

func bDoc(v any) *types.Document {
	return must.NotFail(types.NewDocument("b", v))
}

func bArr(vals ...any) *types.Array {
	docs := make([]any, len(vals))
	for i, v := range vals {
		docs[i] = bDoc(v)
	}
	return must.NotFail(types.NewArray(docs...))
}

func TestDottedIndexEntriesFollowUpdates(t *testing.T) {
	ctx := context.Background()
	b, c := newDottedPathTestBackend(t, backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a.b"}}})
	raw := rawCollection(b, "idxdb", "tests")

	if err := insertDottedPathDoc(c, abDoc(1, bDoc(int32(1)))); err != nil {
		t.Fatal(err)
	}
	expectHolders(t, ctx, raw, "a_b", int32(1), []int32{1}, 1)

	updateDoc(t, ctx, c, abDoc(1, bDoc(int32(2))))
	expectHolders(t, ctx, raw, "a_b", int32(1), []int32{}, 1)
	expectHolders(t, ctx, raw, "a_b", int32(2), []int32{1}, 1)

	updateDoc(t, ctx, c, abDoc(1, bArr(int32(3), int32(4))))
	expectHolders(t, ctx, raw, "a_b", int32(2), []int32{}, 1)
	expectHolders(t, ctx, raw, "a_b", int32(3), []int32{1}, 1)
	expectHolders(t, ctx, raw, "a_b", int32(4), []int32{1}, 1)

	updateDoc(t, ctx, c, abDoc(1, int32(5)))
	expectHolders(t, ctx, raw, "a_b", int32(3), []int32{}, 1)
	expectHolders(t, ctx, raw, "a_b", int32(4), []int32{}, 1)
	expectHolders(t, ctx, raw, "a_b", types.Null, []int32{1}, 1)
}

func TestDottedIndexEntriesRemovedOnDelete(t *testing.T) {
	ctx := context.Background()
	b, c := newDottedPathTestBackend(t, backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a.b"}}})
	raw := rawCollection(b, "idxdb", "tests")
	for _, d := range []*types.Document{abDoc(1, bArr("x", "y")), abDoc(2, bDoc("x"))} {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatal(err)
		}
	}
	expectHolders(t, ctx, raw, "a_b", "x", []int32{1, 2}, 1, 2)

	if _, err := c.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{int32(1)}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	expectHolders(t, ctx, raw, "a_b", "x", []int32{2}, 1, 2)
	expectHolders(t, ctx, raw, "a_b", "y", []int32{}, 1, 2)
}

func TestDottedIndexMultikeyFlag(t *testing.T) {
	ctx := context.Background()
	b, c := newDottedPathTestBackend(t, backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a.b"}}})
	raw := rawCollection(b, "idxdb", "tests")

	if err := insertDottedPathDoc(c, abDoc(1, bDoc(int32(1)))); err != nil {
		t.Fatal(err)
	}
	if info, _ := resolvedIndex(t, ctx, raw, "a_b"); info.Multikey {
		t.Fatal("scalar nested value must not mark the index multikey")
	}

	if err := insertDottedPathDoc(c, abDoc(2, bArr(int32(2), int32(3)))); err != nil {
		t.Fatal(err)
	}
	if info, _ := resolvedIndex(t, ctx, raw, "a_b"); !info.Multikey {
		t.Fatal("array at an intermediate path level must mark the index multikey")
	}
}

// A scalar intermediate leaves a.b absent, so it is skipped; an explicit null is
// present and indexed. Expectations were recorded against MongoDB 8.0.
func TestUniqueSparseIndexDottedPathExplicitNull(t *testing.T) {
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name: "a_b", Unique: true, Sparse: true, Key: []backends.IndexKeyPair{{Field: "a.b"}},
	})
	for _, d := range []*types.Document{
		abDoc(1, int32(5)),
		abDoc(2, int32(5)),
		abDoc(3, must.NotFail(types.NewDocument("c", int32(1)))),
		abDoc(4, must.NotFail(types.NewDocument("c", int32(1)))),
		abDoc(5, bDoc(types.Null)),
	} {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatalf("insert %v: %v", d, err)
		}
	}
	if err := insertDottedPathDoc(c, abDoc(6, bDoc(types.Null))); !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		t.Fatalf("second explicit null on a.b must collide, got %v", err)
	}
}

// Both branches add the same nested unique key: the merge reports a unique
// collision naming the dotted key, keeps ours, and indexes theirs' other docs.
func TestMergeUniqueCollisionOnDottedPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", abDoc(1, bDoc("seed")))
	main := collAt(t, b, "testdb", "main", "items")
	if _, err := main.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{Name: "a_b_unique", Unique: true, Key: []backends.IndexKeyPair{{Field: "a.b"}}}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	insertOne(t, ctx, collAt(t, b, "testdb", "main", "items"), abDoc(10, bDoc("dup")))
	commitBranch(t, b, "testdb", "main", "main: a.b=dup")

	feat := collAt(t, b, "testdb", "feat", "items")
	insertOne(t, ctx, feat, abDoc(20, bDoc("dup")))
	insertOne(t, ctx, feat, abDoc(30, bArr("x", "y")))
	commitBranch(t, b, "testdb", "feat", "feat: a.b=dup and a.b=[x y]")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(conflicts))
	}
	reason := conflicts[0].Reason
	if reason.Code != "uniqueKeyCollision" || reason.Index != "a_b_unique" {
		t.Fatalf("conflict reason = %+v, want uniqueKeyCollision on a_b_unique", reason)
	}
	if got, _ := reason.Key.Get("a.b"); got != "dup" {
		t.Fatalf("conflict key a.b = %v, want dup", got)
	}

	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "ours", nil); err != nil {
		t.Fatalf("resolve ours: %v", err)
	}
	continueMerge(t, b, "testdb", "main")

	merged := rawCollection(b, "testdb", "items")
	expectHolders(t, ctx, merged, "a_b_unique", "dup", []int32{10}, 10, 20)
	expectHolders(t, ctx, merged, "a_b_unique", "x", []int32{30}, 30)
	expectHolders(t, ctx, merged, "a_b_unique", "y", []int32{30}, 30)
	expectHolders(t, ctx, merged, "a_b_unique", "seed", []int32{1}, 1)
}

// Every subdocument and every array encodes to one index marker, so the
// index-driven distinct scan must read each document in such a group.
func TestDistinctScanReadsEveryMarkerGroupMember(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{Name: "y", Key: []backends.IndexKeyPair{{Field: "y"}}})
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	for _, d := range []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "y", must.NotFail(types.NewDocument("p", int32(1))))),
		must.NotFail(types.NewDocument("_id", int32(2), "y", must.NotFail(types.NewDocument("p", int32(2))))),
		must.NotFail(types.NewDocument("_id", int32(3), "y", arr(arr(int32(7)), arr(int32(8))))),
		must.NotFail(types.NewDocument("_id", int32(4), "y", arr())),
	} {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.(backends.DistinctScanner).DistinctScan(ctx, &backends.DistinctParams{Key: "y"})
	if err != nil {
		t.Fatalf("DistinctScan: %v", err)
	}
	want := []any{
		must.NotFail(types.NewDocument("p", int32(1))),
		must.NotFail(types.NewDocument("p", int32(2))),
		arr(int32(7)),
		arr(int32(8)),
		types.Undefined,
	}
	for _, w := range want {
		found := false
		for _, v := range res.Values {
			if types.Identical(v, w) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("DistinctScan values %v missing %v", res.Values, w)
		}
	}
}

func queryCandidateIDs(t *testing.T, ctx context.Context, coll backends.Collection, filter *types.Document) []int32 {
	t.Helper()
	res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer res.Iter.Close()
	ids := []int32{}
	for {
		_, doc, err := res.Iter.Next()
		if err != nil {
			break
		}
		ids = append(ids, must.NotFail(doc.Get("_id")).(int32))
	}
	slices.Sort(ids)
	return ids
}

// The backend returns only index candidates for a dotted filter rather than
// every document (the handler re-filters candidates).
func TestDottedFilterUsesIndex(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a.b"}}})
	for i := int32(1); i <= 10; i++ {
		if err := insertDottedPathDoc(c, abDoc(i, bDoc(i))); err != nil {
			t.Fatal(err)
		}
	}

	eq := must.NotFail(types.NewDocument("a.b", int32(3)))
	if got := queryCandidateIDs(t, ctx, c, eq); !slices.Equal(got, []int32{3}) {
		t.Fatalf("equality candidates = %v, want [3]", got)
	}
	in := must.NotFail(types.NewDocument("a.b", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(int32(2), int32(7)))))))
	if got := queryCandidateIDs(t, ctx, c, in); !slices.Equal(got, []int32{2, 7}) {
		t.Fatalf("$in candidates = %v, want [2 7]", got)
	}

	res, err := c.Count(ctx, &backends.CountParams{Filter: eq})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if !res.Filtered || res.Count != 1 {
		t.Fatalf("Count = %+v, want an index-answered count of 1", res)
	}
}

// On a multikey field each range operator may be met by a different element, so
// the index scan must not intersect the bounds. Expectations were recorded
// against MongoDB 8.0.
func TestMultikeyRangeDoesNotIntersectBounds(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a.b"}}})
	for _, d := range []*types.Document{
		abDoc(1, bDoc(must.NotFail(types.NewArray(int32(1), int32(3))))),
		abDoc(2, bArr(int32(1), int32(4))),
		abDoc(3, bDoc(int32(9))),
	} {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatal(err)
		}
	}
	rng := must.NotFail(types.NewDocument("a.b", must.NotFail(types.NewDocument("$gt", int32(2), "$lt", int32(3)))))
	got := queryCandidateIDs(t, ctx, c, rng)
	for _, want := range []int32{1, 2} {
		if !slices.Contains(got, want) {
			t.Fatalf("range candidates = %v, missing %d", got, want)
		}
	}
}
