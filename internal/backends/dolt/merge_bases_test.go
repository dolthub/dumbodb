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
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"
)

// The tests in this file swap the commit clock, so they must not run in parallel.

// useTestCommitClock makes every commit one second newer than the one before, so that later commits have later
// committer dates.
func useTestCommitClock(t *testing.T) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ticks atomic.Int64
	previous := datas.CommitNow
	datas.CommitNow = func() time.Time {
		return start.Add(time.Duration(ticks.Add(1)) * time.Second)
	}
	t.Cleanup(func() { datas.CommitNow = previous })
}

func editDocOnBranch(t *testing.T, b *Backend, branch string, id int32, value string) {
	t.Helper()
	updateDocOnBranch(t, b, "testdb", branch, "items", mustDoc(t, "_id", id, "v", value))
}

func branchHash(t *testing.T, b *Backend, branch string) hash.Hash {
	t.Helper()
	db, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	h, err := resolveRootishToCommitHash(context.Background(), db, branch)
	require.NoError(t, err)
	return h
}

func requireMergeBases(t *testing.T, b *Backend, ours, theirs string, expected ...string) {
	t.Helper()
	db, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	bases, err := db.mergeBases(context.Background(), branchHash(t, b, ours), branchHash(t, b, theirs))
	require.NoError(t, err)
	want := make([]hash.Hash, len(expected))
	for i, branch := range expected {
		want[i] = branchHash(t, b, branch)
	}
	require.Equal(t, want, bases)
}

// seedItems commits c0 on main with documents 1 to 4.
func seedItems(t *testing.T, b *Backend) {
	t.Helper()
	for id := int32(1); id <= 4; id++ {
		insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", id, "v", "seed"))
	}
	commitDB(t, b, "testdb", "c0")
}

// A criss-cross, the smallest history with two merge bases:
//
//	c0 -- x1 -- x2    x
//	  \-- y1 -- y2    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)
//
// x1 edits document 1 and y1 document 2. y1 is committed after x1, so it is listed first.
func TestMergeBases_CrissCross(t *testing.T) {
	useTestCommitClock(t)
	b := newTestBackend(t)
	seedItems(t, b)
	branchFrom(t, b, "testdb", "main", "x")
	branchFrom(t, b, "testdb", "main", "y")

	editDocOnBranch(t, b, "x", 1, "x1")
	commitBranch(t, b, "testdb", "x", "x1")
	branchFrom(t, b, "testdb", "x", "x1")
	editDocOnBranch(t, b, "y", 2, "y1")
	commitBranch(t, b, "testdb", "y", "y1")
	branchFrom(t, b, "testdb", "y", "y1")

	mergeBranches(t, b, "testdb", "x", "y1")
	mergeBranches(t, b, "testdb", "y", "x1")

	requireMergeBases(t, b, "x", "y", "y1", "x1")
	requireMergeBases(t, b, "y", "x", "y1", "x1")
}

// m1 and m2 each merge p1, p2 and p3, in opposite orders, so they have three merge bases:
//
//	c0 -- m1start -- merge p1 -- merge p2 -- merge p3    m1
//	 |\-- m2start -- merge p3 -- merge p2 -- merge p1    m2
//	 |\-- p1
//	 |\-- p2
//	 \-- p3
//
// p1, p2 and p3 edit documents 1, 2 and 3, and are committed in that order.
func TestMergeBases_ThreeMergeBases(t *testing.T) {
	useTestCommitClock(t)
	b := newTestBackend(t)
	seedItems(t, b)
	for _, branch := range []string{"p1", "p2", "p3", "m1", "m2"} {
		branchFrom(t, b, "testdb", "main", branch)
	}
	for i, p := range []string{"p1", "p2", "p3"} {
		editDocOnBranch(t, b, p, int32(i+1), p)
		commitBranch(t, b, "testdb", p, p)
	}
	insertDoc(t, b, "testdb@m1", "other", mustDoc(t, "_id", int32(10)))
	commitBranch(t, b, "testdb", "m1", "m1start")
	insertDoc(t, b, "testdb@m2", "other", mustDoc(t, "_id", int32(20)))
	commitBranch(t, b, "testdb", "m2", "m2start")
	for _, p := range []string{"p1", "p2", "p3"} {
		mergeBranches(t, b, "testdb", "m1", p)
	}
	for _, p := range []string{"p3", "p2", "p1"} {
		mergeBranches(t, b, "testdb", "m2", p)
	}

	requireMergeBases(t, b, "m1", "m2", "p3", "p2", "p1")
}

// An ordinary fork has one merge base, the fork point:
//
//	c0 -- f -- a1    a
//	       \-- b1    b
func TestMergeBases_Fork(t *testing.T) {
	useTestCommitClock(t)
	b := newTestBackend(t)
	seedItems(t, b)
	editDocOnBranch(t, b, "main", 1, "f")
	commitBranch(t, b, "testdb", "main", "f")
	branchFrom(t, b, "testdb", "main", "fork")
	branchFrom(t, b, "testdb", "main", "a")
	branchFrom(t, b, "testdb", "main", "b")
	editDocOnBranch(t, b, "a", 2, "a1")
	commitBranch(t, b, "testdb", "a", "a1")
	editDocOnBranch(t, b, "b", 3, "b1")
	commitBranch(t, b, "testdb", "b", "b1")

	requireMergeBases(t, b, "a", "b", "fork")
}

// When one commit is an ancestor of the other, it is their only merge base:
//
//	c0 -- d1 -- d2    d
func TestMergeBases_Ancestor(t *testing.T) {
	useTestCommitClock(t)
	b := newTestBackend(t)
	seedItems(t, b)
	branchFrom(t, b, "testdb", "main", "d")
	editDocOnBranch(t, b, "d", 1, "d1")
	commitBranch(t, b, "testdb", "d", "d1")
	branchFrom(t, b, "testdb", "d", "d1")
	editDocOnBranch(t, b, "d", 2, "d2")
	commitBranch(t, b, "testdb", "d", "d2")

	requireMergeBases(t, b, "d", "d1", "d1")
	requireMergeBases(t, b, "d1", "d", "d1")
}
