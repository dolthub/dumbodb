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
	"reflect"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

// commitTS commits the working set of (db, branch) with an explicit author
// timestamp so topological tie-breaks (equal height) are deterministic.
func commitTS(t *testing.T, b *Backend, db, branch, msg string, ms int64) string {
	t.Helper()
	res, err := b.DumboDBCommit(context.Background(), &backends.CommitParams{
		DBName:    db,
		Branch:    branch,
		Message:   msg,
		Author:    "t <t@t.io>",
		Timestamp: time.UnixMilli(ms),
	})
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return res.CommitID
}

// nameMap inverts a message->hash map so log output can be asserted on stable
// messages rather than content-addressed hashes. Unknown hashes (the auto
// "Initialize database" root) map to "init".
func nameMap(byMsg map[string]string) map[string]string {
	byHash := map[string]string{}
	for msg, h := range byMsg {
		byHash[h] = msg
	}
	return byHash
}

func names(byHash map[string]string, hashes []string) []string {
	out := make([]string, len(hashes))
	for i, h := range hashes {
		if n, ok := byHash[h]; ok {
			out[i] = n
		} else {
			out[i] = "init"
		}
	}
	return out
}

func logOnce(t *testing.T, b *Backend, db, branch string, limit int32, from []string) *backends.LogResult {
	t.Helper()
	res, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: db, Branch: branch, ConnBranch: branch, Limit: limit, From: from,
	})
	if err != nil {
		t.Fatalf("DumboDBLog(from=%v): %v", from, err)
	}
	return res
}

// fullWalk returns every commit id from a single large page.
func fullWalk(t *testing.T, b *Backend, db, branch string) []string {
	t.Helper()
	res := logOnce(t, b, db, branch, 1000, nil)
	if len(res.Next) != 0 {
		t.Fatalf("fullWalk: expected empty Next, got %v", res.Next)
	}
	ids := make([]string, len(res.Commits))
	for i, c := range res.Commits {
		ids[i] = c.CommitID
	}
	return ids
}

// pageAll pages through the whole history at the given limit, feeding Next back
// as From. It asserts no commit is emitted twice and returns the concatenation.
func pageAll(t *testing.T, b *Backend, db, branch string, limit int32) []string {
	t.Helper()
	var all []string
	seen := map[string]bool{}
	var from []string
	for i := 0; ; i++ {
		if i > 10000 {
			t.Fatalf("pageAll: did not terminate")
		}
		res := logOnce(t, b, db, branch, limit, from)
		for _, c := range res.Commits {
			if seen[c.CommitID] {
				t.Fatalf("pageAll: commit %s emitted twice", c.CommitID)
			}
			seen[c.CommitID] = true
			all = append(all, c.CommitID)
		}
		if len(res.Next) == 0 {
			break
		}
		from = res.Next
	}
	return all
}

// buildLinear creates n commits c1..cn on main. Returns the message->hash map.
func buildLinear(t *testing.T, b *Backend, db string, n int) map[string]string {
	t.Helper()
	byMsg := map[string]string{}
	for i := 1; i <= n; i++ {
		insertOne(t, context.Background(), collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(i)))
		msg := fmt.Sprintf("c%d", i)
		byMsg[msg] = commitTS(t, b, db, "main", msg, int64(i)*1000)
	}
	return byMsg
}

// buildDormant creates the verified dormant-branch graph:
//
//	init - m1 - m2 - m3 - m4 - m5 - M
//	            \                   /
//	             f1 ------- f2 ------
//
// Feature commits get older timestamps than the later main commits, so equal
// heights resolve main-first. Confirmed full walk: M m5 m4 f2 m3 f1 m2 m1 init.
func buildDormant(t *testing.T, b *Backend, db string) map[string]string {
	t.Helper()
	ctx := context.Background()
	byMsg := map[string]string{}

	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(1)))
	byMsg["m1"] = commitTS(t, b, db, "main", "m1", 10_000)
	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(2)))
	byMsg["m2"] = commitTS(t, b, db, "main", "m2", 20_000)

	branchFrom(t, b, db, "main", "feat")
	insertOne(t, ctx, collAt(t, b, db, "feat", "coll"), mustDoc(t, "_id", int64(101)))
	byMsg["f1"] = commitTS(t, b, db, "feat", "f1", 30_000)
	insertOne(t, ctx, collAt(t, b, db, "feat", "coll"), mustDoc(t, "_id", int64(102)))
	byMsg["f2"] = commitTS(t, b, db, "feat", "f2", 40_000)

	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(3)))
	byMsg["m3"] = commitTS(t, b, db, "main", "m3", 50_000)
	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(4)))
	byMsg["m4"] = commitTS(t, b, db, "main", "m4", 60_000)
	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(5)))
	byMsg["m5"] = commitTS(t, b, db, "main", "m5", 70_000)

	mres, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: db, Into: "main", From: "feat", NoFF: true, Author: "t <t@t.io>", Message: "M",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	byMsg["M"] = mres.CommitID
	return byMsg
}

func TestLogPagination_LinearTiling(t *testing.T) {
	b := newTestBackend(t)
	byMsg := buildLinear(t, b, "lin", 8)
	byHash := nameMap(byMsg)

	full := fullWalk(t, b, "lin", "main")
	if got := names(byHash, full); !reflect.DeepEqual(got, []string{"c8", "c7", "c6", "c5", "c4", "c3", "c2", "c1", "init"}) {
		t.Fatalf("full walk order: %v", got)
	}

	for _, limit := range []int32{1, 2, 3, 5, 7, 50} {
		paged := pageAll(t, b, "lin", "main", limit)
		if !reflect.DeepEqual(paged, full) {
			t.Fatalf("limit=%d: paged %v != full %v", limit, names(byHash, paged), names(byHash, full))
		}
	}

	// On a linear history the frontier is always a single commit until the end.
	p := logOnce(t, b, "lin", "main", 3, nil)
	if len(p.Next) != 1 {
		t.Fatalf("linear next should be single commit, got %v", p.Next)
	}
}

func TestLogPagination_Diamond(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "dia"
	byMsg := map[string]string{}

	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(1)))
	byMsg["B"] = commitTS(t, b, db, "main", "B", 10_000)
	branchFrom(t, b, db, "main", "feat")
	insertOne(t, ctx, collAt(t, b, db, "main", "coll"), mustDoc(t, "_id", int64(2)))
	byMsg["L"] = commitTS(t, b, db, "main", "L", 20_000)
	insertOne(t, ctx, collAt(t, b, db, "feat", "coll"), mustDoc(t, "_id", int64(3)))
	byMsg["R"] = commitTS(t, b, db, "feat", "R", 30_000)
	mres, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: db, Into: "main", From: "feat", NoFF: true, Author: "t <t@t.io>", Message: "M",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	byMsg["M"] = mres.CommitID
	byHash := nameMap(byMsg)

	full := fullWalk(t, b, db, "main")
	for _, limit := range []int32{1, 2, 3, 50} {
		paged := pageAll(t, b, db, "main", limit)
		if !reflect.DeepEqual(paged, full) {
			t.Fatalf("diamond limit=%d: paged %v != full %v", limit, names(byHash, paged), names(byHash, full))
		}
	}
}

func TestLogPagination_DormantBranch(t *testing.T) {
	b := newTestBackend(t)
	byMsg := buildDormant(t, b, "dorm")
	byHash := nameMap(byMsg)

	full := fullWalk(t, b, "dorm", "main")
	wantOrder := []string{"M", "m5", "m4", "f2", "m3", "f1", "m2", "m1", "init"}
	if got := names(byHash, full); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("dormant full walk order: %v", got)
	}

	// Headline: page 1 at limit=2 returns [M, m5] and the frontier carries the
	// dormant feature tip f2 alongside the main frontier m4.
	p1 := logOnce(t, b, "dorm", "main", 2, nil)
	if got := names(byHash, idsOf(p1.Commits)); !reflect.DeepEqual(got, []string{"M", "m5"}) {
		t.Fatalf("page1 commits: %v", got)
	}
	nextNames := map[string]bool{}
	for _, h := range p1.Next {
		nextNames[byHash[h]] = true
	}
	if !nextNames["f2"] || !nextNames["m4"] || len(p1.Next) != 2 {
		t.Fatalf("page1 next should be {f2,m4}, got %v", names(byHash, p1.Next))
	}

	for _, limit := range []int32{1, 2, 3, 5, 7, 50} {
		paged := pageAll(t, b, "dorm", "main", limit)
		if !reflect.DeepEqual(paged, full) {
			t.Fatalf("dormant limit=%d: paged %v != full %v", limit, names(byHash, paged), names(byHash, full))
		}
	}
}

func TestLogPagination_OverlappingSeeds(t *testing.T) {
	b := newTestBackend(t)
	byMsg := buildLinear(t, b, "ov", 6)
	byHash := nameMap(byMsg)

	// Seed with a commit and one of its ancestors; the iterator must dedupe so
	// the ancestor is emitted once. Result equals walking from the descendant.
	res := logOnce(t, b, "ov", "main", 100, []string{byMsg["c5"], byMsg["c3"]})
	got := names(byHash, idsOf(res.Commits))
	want := []string{"c5", "c4", "c3", "c2", "c1", "init"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlapping seeds: got %v want %v", got, want)
	}
	if len(res.Next) != 0 {
		t.Fatalf("overlapping seeds next should be empty, got %v", names(byHash, res.Next))
	}
}

func TestLogPagination_Exhaustion(t *testing.T) {
	b := newTestBackend(t)
	buildLinear(t, b, "ex", 4)

	// Whole history fits: no Next.
	res := logOnce(t, b, "ex", "main", 100, nil)
	if len(res.Next) != 0 {
		t.Fatalf("expected empty Next when history fits, got %v", res.Next)
	}
	// Exact fit: 5 commits (4 + init) at limit 5 -> one page, no Next.
	res = logOnce(t, b, "ex", "main", 5, nil)
	if len(res.Commits) != 5 || len(res.Next) != 0 {
		t.Fatalf("exact fit: %d commits, next=%v", len(res.Commits), res.Next)
	}
}

func TestLogPagination_Determinism(t *testing.T) {
	b := newTestBackend(t)
	buildDormant(t, b, "det")

	a := pageAll(t, b, "det", "main", 2)
	c := pageAll(t, b, "det", "main", 2)
	if !reflect.DeepEqual(a, c) {
		t.Fatalf("pagination not deterministic:\n%v\n%v", a, c)
	}
	n1 := logOnce(t, b, "det", "main", 2, nil).Next
	n2 := logOnce(t, b, "det", "main", 2, nil).Next
	if !reflect.DeepEqual(n1, n2) {
		t.Fatalf("next ordering not deterministic: %v vs %v", n1, n2)
	}
}

func idsOf(commits []backends.CommitInfo) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.CommitID
	}
	return out
}

// TestLogAll_SeedsAllBranchHeads verifies that All seeds the walk with every
// branch HEAD, so commits unique to a non-connection branch appear.
func TestLogAll_SeedsAllBranchHeads(t *testing.T) {
	b := newTestBackend(t)
	byMsg := buildDormant(t, b, "alldb") // main: m1..m5,M ; feat: f1,f2 (f2 NOT merge-reachable? it is via M)
	byHash := nameMap(byMsg)

	// Build a second branch with a commit that is NOT reachable from main HEAD.
	branchFrom(t, b, "alldb", "main", "side")
	insertOne(t, context.Background(), collAt(t, b, "alldb", "side", "coll"), mustDoc(t, "_id", int64(900)))
	byMsg["s1"] = commitTS(t, b, "alldb", "side", "s1", 80_000)
	byHash = nameMap(byMsg)

	// Default walk from main HEAD does NOT include s1.
	def := logOnce(t, b, "alldb", "main", 1000, nil)
	for _, c := range def.Commits {
		if byHash[c.CommitID] == "s1" {
			t.Fatal("default walk should not include the side-branch-only commit")
		}
	}

	// All walk includes s1 (reachable from the 'side' branch head).
	all, err := b.DumboDBLog(context.Background(), &backends.LogParams{
		DBName: "alldb", Branch: "main", ConnBranch: "main", Limit: 1000, All: true,
	})
	if err != nil {
		t.Fatalf("DumboDBLog all: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range all.Commits {
		seen[byHash[c.CommitID]] = true
	}
	if !seen["s1"] {
		t.Fatal("all walk must include the side-branch commit s1")
	}
	if !seen["M"] || !seen["f1"] {
		t.Fatal("all walk must still include main/feature commits")
	}
}
