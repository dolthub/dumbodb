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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

// mergeModes are the merge modes every scenario runs under.
var mergeModes = backends.MergeModeNames()

func createItems(t *testing.T, b *Backend, mode string) {
	t.Helper()
	db, err := b.Database("testdb")
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(context.Background(), &backends.CreateCollectionParams{Name: "items", MergeMode: mode}))
}

// itemValues returns the v field of each document of items on |branch|, by _id.
func itemValues(t *testing.T, b *Backend, branch string) map[int32]any {
	t.Helper()
	values := make(map[int32]any)
	for _, doc := range drainQuery(t, context.Background(), collAt(t, b, "testdb", branch, "items"), &backends.QueryParams{}) {
		id, _ := doc.Get("_id")
		v, _ := doc.Get("v")
		values[id.(int32)] = v
	}
	return values
}

// schemaBranchHistory builds the history from https://github.com/dolthub/dolt/issues/12050 on collection items in
// |mode|. A schema branch changes only metadata and is merged into main and into feature, so main and feature have
// two merge bases, the fork point and the schema tip s2:
//
//	c1 -- c2 [-- c2-3 -- c2-4] -- c3 -- m    main
//	 |                 \-- f1 -- f          feature
//	 \-- s1 -- s2                           schema
//	m = merge(c3, s2), f = merge(f1, s2)
//
// c2 seeds documents 1 and 2, and |extraSeeds| more commits seed documents 3 and 4, so the fork point is c2, c2-3 or
// c2-4. c3 edits document 1 and deletes document 2. f1 adds document 50. s1 and s2 add indexes, after f1, or before
// c2 when |schemaFirst|. A single merge base could be the schema tip, which has no documents.
func schemaBranchHistory(t *testing.T, b *Backend, mode string, extraSeeds int, schemaFirst bool) {
	t.Helper()
	ctx := context.Background()
	schemaCommits := func() {
		createIndex(t, ctx, collAt(t, b, "testdb", "schema", "items"), "by_v", "v")
		commitBranch(t, b, "testdb", "schema", "s1")
		createIndex(t, ctx, collAt(t, b, "testdb", "schema", "items"), "by_w", "w")
		commitBranch(t, b, "testdb", "schema", "s2")
	}
	createItems(t, b, mode)
	commitDB(t, b, "testdb", "c1")
	branchFrom(t, b, "testdb", "main", "schema")
	if schemaFirst {
		schemaCommits()
	}

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "v", "seed"))
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(2), "v", "seed"))
	commitDB(t, b, "testdb", "c2")
	for i := int32(3); i < 3+int32(extraSeeds); i++ {
		insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", i, "v", "seed"))
		commitDB(t, b, "testdb", fmt.Sprintf("c2-%d", i))
	}
	branchFrom(t, b, "testdb", "main", "fork")
	branchFrom(t, b, "testdb", "main", "feature")

	editDocOnBranch(t, b, "main", 1, "edited on main")
	deleteID(t, b, "testdb", "main", "items", int32(2))
	commitDB(t, b, "testdb", "c3")

	insertDoc(t, b, "testdb@feature", "items", mustDoc(t, "_id", int32(50), "v", "feature"))
	commitBranch(t, b, "testdb", "feature", "f1")

	if !schemaFirst {
		schemaCommits()
	}
	mergeBranches(t, b, "testdb", "main", "schema")
	mergeBranches(t, b, "testdb", "feature", "schema")
	if schemaFirst {
		requireMergeBases(t, b, "main", "feature", "fork", "schema")
	} else {
		requireMergeBases(t, b, "main", "feature", "schema", "fork")
	}
}

// A1: main edited document 1 and deleted document 2 since the fork point, and feature added document 50, so the
// merge is clean in every mode. With the schema tip as its only merge base, document 1 would look added on both
// sides, differently, and document 2 added on feature. A2 (Dolt 3a-3c) gives the same result with a fork point as
// tall as the schema tip, taller, and taller and newer.
func TestVirtualMergeBase_SchemaBranch(t *testing.T) {
	histories := []struct {
		name        string
		extraSeeds  int
		schemaFirst bool
	}{
		{"A1 schema tip taller and newer", 0, false},
		{"A2 fork point as tall", 1, false},
		{"A2 fork point taller", 2, false},
		{"A2 fork point taller and newer", 2, true},
	}
	for _, h := range histories {
		want := map[int32]any{1: "edited on main", 50: "feature"}
		for i := int32(3); i < 3+int32(h.extraSeeds); i++ {
			want[i] = "seed"
		}
		for _, mode := range mergeModes {
			for _, direction := range []struct{ into, from string }{{"feature", "main"}, {"main", "feature"}} {
				t.Run(h.name+"/"+mode+"/merge "+direction.from+" into "+direction.into, func(t *testing.T) {
					useTestCommitClock(t)
					b := newTestBackend(t)
					schemaBranchHistory(t, b, mode, h.extraSeeds, h.schemaFirst)
					mergeBranches(t, b, "testdb", direction.into, direction.from)
					require.Equal(t, want, itemValues(t, b, direction.into))
				})
			}
		}
	}
}
