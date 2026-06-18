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
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
)

func init() {
	// The backend evaluates _id membership through the registered find()
	// matcher (an internal {_id: {$in: [...]}} filter); wire it up for tests.
	backends.RegisterPartialFilterMatcher(common.FilterDocument)
}

func updateDoc(t *testing.T, b *Backend, db, branch, coll string, doc *types.Document) {
	t.Helper()
	if _, err := collAt(t, b, db, branch, coll).UpdateAll(context.Background(),
		&backends.UpdateAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}
}

func deleteID(t *testing.T, b *Backend, db, branch, coll string, id any) {
	t.Helper()
	if _, err := collAt(t, b, db, branch, coll).DeleteAll(context.Background(),
		&backends.DeleteAllParams{IDs: []any{id}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
}

// idFilters is a test convenience: collection -> _id list, where an empty list
// means the whole-collection wildcard.
func idFilters(m map[string][]any) map[string]backends.CommitFilter {
	out := make(map[string]backends.CommitFilter, len(m))
	for coll, ids := range m {
		if len(ids) == 0 {
			out[coll] = backends.CommitFilter{All: true}
		} else {
			out[coll] = backends.CommitFilter{IDs: ids}
		}
	}
	return out
}

func logFilter(t *testing.T, b *Backend, db string, filters map[string][]any) *backends.LogResult {
	t.Helper()
	res, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: db, Branch: "main", ConnBranch: "main", Limit: 100, Filters: idFilters(filters),
	})
	if err != nil {
		t.Fatalf("DumboDBLog(filter): %v", err)
	}
	return res
}

// buildFilterHistory: orders o1/o2/o3, users u1/u2, with a mixed commit.
//
//	c1 add orders o1,o2
//	c2 add users u1,u2
//	c3 add order o3
//	c4 modify o1 (+note) + o2 (+region) + u2 (rename)   [mixed]
//	c5 delete o1
//	c6 users-only (rename u1)
func buildFilterHistory(t *testing.T, b *Backend, db string) map[string]string {
	t.Helper()
	ctx := context.Background()
	h := map[string]string{}
	oc := func() backends.Collection { return collAt(t, b, db, "main", "orders") }
	uc := func() backends.Collection { return collAt(t, b, db, "main", "users") }

	insertOne(t, ctx, oc(), mustDoc(t, "_id", int64(1), "status", "pending"))
	insertOne(t, ctx, oc(), mustDoc(t, "_id", int64(2), "status", "shipped"))
	h["c1"] = commitTS(t, b, db, "main", "c1", 10_000)

	insertOne(t, ctx, uc(), mustDoc(t, "_id", int64(1), "name", "alice"))
	insertOne(t, ctx, uc(), mustDoc(t, "_id", int64(2), "name", "bob"))
	h["c2"] = commitTS(t, b, db, "main", "c2", 20_000)

	insertOne(t, ctx, oc(), mustDoc(t, "_id", int64(3), "status", "pending"))
	h["c3"] = commitTS(t, b, db, "main", "c3", 30_000)

	updateDoc(t, b, db, "main", "orders", mustDoc(t, "_id", int64(1), "status", "pending", "note", "x"))
	updateDoc(t, b, db, "main", "orders", mustDoc(t, "_id", int64(2), "status", "shipped", "region", "eu"))
	updateDoc(t, b, db, "main", "users", mustDoc(t, "_id", int64(2), "name", "bobby"))
	h["c4"] = commitTS(t, b, db, "main", "c4", 40_000)

	deleteID(t, b, db, "main", "orders", int64(1))
	h["c5"] = commitTS(t, b, db, "main", "c5", 50_000)

	updateDoc(t, b, db, "main", "users", mustDoc(t, "_id", int64(1), "name", "alicia"))
	h["c6"] = commitTS(t, b, db, "main", "c6", 60_000)

	return h
}

func TestLogIDFilter_FollowOneDocument(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "f1")
	byHash := nameMap(h)

	// orders/_id:1 touched by c1 (add), c4 (modify), c5 (delete). Not c3 (o3).
	got := names(byHash, idsOf(logFilter(t, b, "f1", map[string][]any{"orders": {int64(1)}}).Commits))
	if want := []string{"c5", "c4", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("follow orders/_id:1: got %v want %v", got, want)
	}
}

func TestLogIDFilter_IDListSugar(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "f2")
	byHash := nameMap(h)

	// orders _id in {1,3}: c5(o1), c4(o1), c3(o3), c1(o1). c2/c6 (users) excluded.
	got := names(byHash, idsOf(logFilter(t, b, "f2", map[string][]any{"orders": {int64(1), int64(3)}}).Commits))
	if want := []string{"c5", "c4", "c3", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("orders _id in {1,3}: got %v want %v", got, want)
	}
}

func TestLogIDFilter_PerCollectionOR(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "f3")
	byHash := nameMap(h)

	// orders/_id:3 OR users/_id:1: c6(u1), c3(o3), c2(u1), c1? c1 added o1,o2 not o3 -> no.
	got := names(byHash, idsOf(logFilter(t, b, "f3", map[string][]any{
		"orders": {int64(3)}, "users": {int64(1)},
	}).Commits))
	if want := []string{"c6", "c3", "c2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OR filter: got %v want %v", got, want)
	}
}

func TestLogIDFilter_ScopeStatPatch(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "f4")
	ctx := context.Background()

	// c4 changed o1 (matched), o2 (not matched), u2 (other collection).
	res, err := b.DumboDBLog(ctx, &backends.LogParams{
		DBName: "f4", Branch: "main", ConnBranch: "main", Limit: 1, From: []string{h["c4"]},
		Filters: idFilters(map[string][]any{"orders": {int64(1)}}), Stat: true, Patch: true,
	})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}
	c := res.Commits[0]
	if len(c.Stat) != 1 || c.Stat[0].Name != "orders" {
		t.Fatalf("stat should report only orders, got %+v", c.Stat)
	}
	if c.Stat[0].Modified != 1 {
		t.Fatalf("stat should count only the matched doc o1, got %d", c.Stat[0].Modified)
	}
	if len(c.Diff) != 1 || c.Diff[0].Name != "orders" {
		t.Fatalf("diff should report only orders, got %+v", c.Diff)
	}
	if len(c.Diff[0].Modified) != 1 || c.Diff[0].Modified[0].ID != int64(1) {
		t.Fatalf("diff should carry only o1, got %+v", c.Diff[0].Modified)
	}
}

func TestLogIDFilter_NumericCoercionAndTypes(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "f5"

	oid := types.NewObjectID()
	insertOne(t, ctx, collAt(t, b, db, "main", "orders"), mustDoc(t, "_id", int64(5)))
	insertOne(t, ctx, collAt(t, b, db, "main", "orders"), mustDoc(t, "_id", "abc"))
	insertOne(t, ctx, collAt(t, b, db, "main", "orders"), mustDoc(t, "_id", oid))
	commitTS(t, b, db, "main", "add", 10_000)

	// int32 filter matches a stored int64 _id (Mongo numeric coercion).
	if n := len(logFilter(t, b, db, map[string][]any{"orders": {int32(5)}}).Commits); n != 1 {
		t.Fatalf("int32 filter should match stored int64 _id, got %d commits", n)
	}
	// string _id.
	if n := len(logFilter(t, b, db, map[string][]any{"orders": {"abc"}}).Commits); n != 1 {
		t.Fatalf("string _id should match, got %d", n)
	}
	// ObjectId _id.
	if n := len(logFilter(t, b, db, map[string][]any{"orders": {oid}}).Commits); n != 1 {
		t.Fatalf("ObjectId _id should match, got %d", n)
	}
	// A non-existent id matches nothing.
	if n := len(logFilter(t, b, db, map[string][]any{"orders": {int64(999)}}).Commits); n != 0 {
		t.Fatalf("missing _id should match nothing, got %d", n)
	}
}

func TestLogIDFilter_LimitCountsMatchesWithNext(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "f6")
	byHash := nameMap(h)
	filter := idFilters(map[string][]any{"orders": {int64(1)}})

	// HEAD is c6 (users only). limit=1 returns c5; walk skips c6; next past it.
	res, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: "f6", Branch: "main", ConnBranch: "main", Limit: 1, Filters: filter,
	})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}
	if got := names(byHash, idsOf(res.Commits)); !reflect.DeepEqual(got, []string{"c5"}) {
		t.Fatalf("page1: got %v", got)
	}
	if len(res.Next) == 0 {
		t.Fatal("expected non-empty next")
	}
	for _, n := range res.Next {
		if byHash[n] == "c6" {
			t.Fatal("examined-but-skipped c6 must not be in next")
		}
	}
}

// TestLogIDFilter_DocumentID covers _ids that are themselves documents
// (subdocument _ids), which MongoDB permits. Field order is significant:
// {a:1,b:"x"} and {b:"x",a:1} are distinct _ids.
func TestLogIDFilter_DocumentID(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "fdoc"
	h := map[string]string{}

	idAB := mustDoc(t, "a", int64(1), "b", "x")  // {a:1, b:"x"}
	idBA := mustDoc(t, "b", "x", "a", int64(1))  // {b:"x", a:1}  (distinct _id)

	insertOne(t, ctx, collAt(t, b, db, "main", "orders"), mustDoc(t, "_id", idAB, "v", int64(1)))
	h["cAB"] = commitTS(t, b, db, "main", "cAB", 10_000)
	insertOne(t, ctx, collAt(t, b, db, "main", "orders"), mustDoc(t, "_id", idBA, "v", int64(2)))
	h["cBA"] = commitTS(t, b, db, "main", "cBA", 20_000)
	// A later modification of the {a:1,b:"x"} doc -- should also be a touch.
	updateDoc(t, b, db, "main", "orders", mustDoc(t, "_id", idAB, "v", int64(99)))
	h["cMod"] = commitTS(t, b, db, "main", "cMod", 30_000)
	byHash := nameMap(h)

	// Filter by the {a:1,b:"x"} _id: matches its insert and modification only.
	got := names(byHash, idsOf(logFilter(t, b, db, map[string][]any{
		"orders": {mustDoc(t, "a", int64(1), "b", "x")},
	}).Commits))
	if want := []string{"cMod", "cAB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("document _id {a,b}: got %v want %v", got, want)
	}

	// Field order is significant: filtering {b:"x",a:1} matches only cBA.
	got = names(byHash, idsOf(logFilter(t, b, db, map[string][]any{
		"orders": {mustDoc(t, "b", "x", "a", int64(1))},
	}).Commits))
	if want := []string{"cBA"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("document _id {b,a} (order-sensitive): got %v want %v", got, want)
	}
}

// TestLogIDFilter_WholeCollection covers the empty-id-list wildcard: match any
// document in the collection (commits that touched that collection at all).
func TestLogIDFilter_WholeCollection(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "fwc")
	byHash := nameMap(h)

	// Any orders change: c5(del o1), c4(o1,o2), c3(o3), c1(o1,o2). Not c2/c6 (users).
	got := names(byHash, idsOf(logFilter(t, b, "fwc", map[string][]any{"orders": {}}).Commits))
	if want := []string{"c5", "c4", "c3", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("whole-collection orders: got %v want %v", got, want)
	}

	// Whole-collection OR a specific id in another collection.
	got = names(byHash, idsOf(logFilter(t, b, "fwc", map[string][]any{
		"orders": {}, "users": {int64(1)},
	}).Commits))
	if want := []string{"c6", "c5", "c4", "c3", "c2", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("orders(all) OR users/1: got %v want %v", got, want)
	}

	// Whole-collection scopes stat to that collection's changed docs. c4 changed
	// both o1 and o2 -> Modified count 2 (vs the _id-scoped case which was 1).
	res, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: "fwc", Branch: "main", ConnBranch: "main", Limit: 1, From: []string{h["c4"]},
		Filters: idFilters(map[string][]any{"orders": {}}), Stat: true,
	})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}
	st := res.Commits[0].Stat
	if len(st) != 1 || st[0].Name != "orders" || st[0].Modified != 2 {
		t.Fatalf("whole-collection stat should report orders with 2 modified, got %+v", st)
	}
}

// logCF runs DumboDBLog with an explicit CommitFilter map (for $match tests).
func logCF(t *testing.T, b *Backend, db string, filters map[string]backends.CommitFilter) *backends.LogResult {
	t.Helper()
	res, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: db, Branch: "main", ConnBranch: "main", Limit: 100, Filters: filters,
	})
	if err != nil {
		t.Fatalf("DumboDBLog($match): %v", err)
	}
	return res
}

// TestLogIDFilter_Match covers $match: a find() predicate applied per commit
// against the parent1 diff (touched semantics). A commit is included when it
// touched a document matching the query (pre- or post-image for modifications).
func TestLogIDFilter_Match(t *testing.T) {
	b := newTestBackend(t)
	h := buildFilterHistory(t, b, "fm")
	byHash := nameMap(h)

	// $match {status:"pending"} touched: c1 (add o1 pending), c3 (add o3),
	// c4 (modify o1, stays pending), c5 (delete o1, pre pending). c2/c6 = users.
	got := names(byHash, idsOf(logCF(t, b, "fm", map[string]backends.CommitFilter{
		"orders": {Queries: []*types.Document{mustDoc(t, "status", "pending")}},
	}).Commits))
	if want := []string{"c5", "c4", "c3", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("$match status=pending (touched): got %v want %v", got, want)
	}

	// Multiple $match OR: pending {c5,c4,c3,c1} OR shipped {c4,c1} -> same union.
	got = names(byHash, idsOf(logCF(t, b, "fm", map[string]backends.CommitFilter{
		"orders": {Queries: []*types.Document{
			mustDoc(t, "status", "shipped"),
			mustDoc(t, "status", "pending"),
		}},
	}).Commits))
	if want := []string{"c5", "c4", "c3", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("$match OR: got %v want %v", got, want)
	}

	// $match shipped alone touched: c1 (add o2 shipped), c4 (modify o2).
	got = names(byHash, idsOf(logCF(t, b, "fm", map[string]backends.CommitFilter{
		"orders": {Queries: []*types.Document{mustDoc(t, "status", "shipped")}},
	}).Commits))
	if want := []string{"c4", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("$match status=shipped (touched): got %v want %v", got, want)
	}

	// $match OR an explicit _id: pending {c5,c4,c3,c1} OR _id:1 {c5,c4,c1}.
	got = names(byHash, idsOf(logCF(t, b, "fm", map[string]backends.CommitFilter{
		"orders": {IDs: []any{int64(1)}, Queries: []*types.Document{mustDoc(t, "status", "pending")}},
	}).Commits))
	if want := []string{"c5", "c4", "c3", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("$match + _id: got %v want %v", got, want)
	}

	// A $match no document ever matched touches nothing.
	got = names(byHash, idsOf(logCF(t, b, "fm", map[string]backends.CommitFilter{
		"orders": {Queries: []*types.Document{mustDoc(t, "status", "cancelled")}},
	}).Commits))
	if len(got) != 0 {
		t.Fatalf("$match matching no touched doc should return nothing, got %v", got)
	}
}
