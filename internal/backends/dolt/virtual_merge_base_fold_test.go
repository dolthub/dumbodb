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
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
)

// itemDoc returns a document of items with fields a and b, an embedded document sub with fields p and q, and an
// array arr.
func itemDoc(t *testing.T, id int32, a, b, p, q string, arr ...int32) *types.Document {
	t.Helper()
	values := make([]any, len(arr))
	for i, v := range arr {
		values[i] = v
	}
	array, err := types.NewArray(values...)
	require.NoError(t, err)
	return mustDoc(t, "_id", id, "a", a, "b", b, "sub", mustDoc(t, "p", p, "q", q), "arr", array)
}

func seedItem(t *testing.T, id int32) *types.Document {
	return itemDoc(t, id, "seed", "seed", "seed", "seed", 1)
}

// foldResult is the document a virtual merge base holds, nil when absent, and its flag, nil when unflagged.
type foldResult struct {
	doc  *types.Document
	flag *docFlag
}

func wholeFlag() *docFlag { return &docFlag{whole: true} }

func pathFlag(paths ...[]string) *docFlag { return &docFlag{paths: paths} }

// foldScenario folds two merge bases, x1 and y1, which edit c0 with |x| and |y|:
//
//	c0 -- x1    x
//	  \-- y1    y
//
// c0 has documents 1 and 2 of seedItem in items. want is the virtual merge base's document |id| and its flag, by
// merge mode. Document 2 is never edited.
type foldScenario struct {
	name string
	id   int32
	x, y func(t *testing.T, b *Backend, branch string)
	want map[string]foldResult
}

func setItem(doc func(t *testing.T) *types.Document) func(t *testing.T, b *Backend, branch string) {
	return func(t *testing.T, b *Backend, branch string) {
		updateDocOnBranch(t, b, "testdb", branch, "items", doc(t))
	}
}

func addItem(doc func(t *testing.T) *types.Document) func(t *testing.T, b *Backend, branch string) {
	return func(t *testing.T, b *Backend, branch string) {
		insertDoc(t, b, "testdb@"+branch, "items", doc(t))
	}
}

func deleteItem(id int32) func(t *testing.T, b *Backend, branch string) {
	return func(t *testing.T, b *Backend, branch string) {
		deleteID(t, b, "testdb", branch, "items", id)
	}
}

func docOf(build func(t *testing.T) *types.Document) func(t *testing.T) *types.Document { return build }

func byMode(dT, fT, fD, dD foldResult) map[string]foldResult {
	return map[string]foldResult{
		"documentTouched": dT, "fieldTouched": fT, "fieldDivergent": fD, "documentDivergent": dD,
	}
}

func foldScenarios(t *testing.T) []foldScenario {
	seed := seedItem(t, 1)
	sameA := itemDoc(t, 1, "same", "seed", "seed", "seed", 1)
	disjoint := itemDoc(t, 1, "x", "y", "seed", "seed", 1)
	nestedDisjoint := itemDoc(t, 1, "seed", "seed", "x", "y", 1)
	added := itemDoc(t, 3, "same", "seed", "seed", "seed", 1)
	return []foldScenario{
		{
			name: "same field, different values",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "x", "seed", "seed", "seed", 1) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "y", "seed", "seed", "seed", 1) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{seed, pathFlag([]string{"a"})},
				foldResult{seed, pathFlag([]string{"a"})}, foldResult{seed, wholeFlag()}),
		},
		{
			name: "same field, same value",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "same", "seed", "seed", "seed", 1) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "same", "seed", "seed", "seed", 1) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{seed, pathFlag([]string{"a"})},
				foldResult{sameA, nil}, foldResult{sameA, nil}),
		},
		{
			name: "different fields",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "x", "seed", "seed", "seed", 1) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "y", "seed", "seed", 1) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{disjoint, nil},
				foldResult{disjoint, nil}, foldResult{seed, wholeFlag()}),
		},
		{
			name: "different fields of an embedded document",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "x", "seed", 1) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "seed", "y", 1) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{nestedDisjoint, nil},
				foldResult{nestedDisjoint, nil}, foldResult{seed, wholeFlag()}),
		},
		{
			name: "same field of an embedded document, different values",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "x", "seed", 1) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "y", "seed", 1) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{seed, pathFlag([]string{"sub", "p"})},
				foldResult{seed, pathFlag([]string{"sub", "p"})}, foldResult{seed, wholeFlag()}),
		},
		{
			name: "array changed differently",
			id:   1,
			x:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "seed", "seed", 1, 2) }),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "seed", "seed", "seed", "seed", 1, 3) }),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{seed, pathFlag([]string{"arr"})},
				foldResult{seed, pathFlag([]string{"arr"})}, foldResult{seed, wholeFlag()}),
		},
		{
			// As in git and Dolt, a delete against a modify keeps the base document, unflagged.
			name: "delete against modify",
			id:   1,
			x:    deleteItem(1),
			y:    setItem(func(t *testing.T) *types.Document { return itemDoc(t, 1, "y", "seed", "seed", "seed", 1) }),
			want: byMode(foldResult{seed, nil}, foldResult{seed, nil}, foldResult{seed, nil}, foldResult{seed, nil}),
		},
		{
			name: "both delete",
			id:   1,
			x:    deleteItem(1),
			y:    deleteItem(1),
			want: byMode(foldResult{seed, wholeFlag()}, foldResult{seed, wholeFlag()},
				foldResult{nil, nil}, foldResult{nil, nil}),
		},
		{
			name: "same _id added differently",
			id:   3,
			x:    addItem(docOf(func(t *testing.T) *types.Document { return itemDoc(t, 3, "x", "seed", "seed", "seed", 1) })),
			y:    addItem(docOf(func(t *testing.T) *types.Document { return itemDoc(t, 3, "y", "seed", "seed", "seed", 1) })),
			want: byMode(foldResult{nil, wholeFlag()}, foldResult{nil, wholeFlag()},
				foldResult{nil, wholeFlag()}, foldResult{nil, wholeFlag()}),
		},
		{
			name: "same _id added alike",
			id:   3,
			x:    addItem(docOf(func(t *testing.T) *types.Document { return itemDoc(t, 3, "same", "seed", "seed", "seed", 1) })),
			y:    addItem(docOf(func(t *testing.T) *types.Document { return itemDoc(t, 3, "same", "seed", "seed", "seed", 1) })),
			want: byMode(foldResult{nil, wholeFlag()}, foldResult{nil, wholeFlag()},
				foldResult{added, nil}, foldResult{added, nil}),
		},
	}
}

// foldMergeBases builds c0, x1 and y1 for |s| in |mode| and returns the virtual merge base of x1 and y1, with its
// flags.
func foldMergeBases(t *testing.T, b *Backend, mode string, s foldScenario) (hash.Hash, virtualBaseFlags) {
	t.Helper()
	createItems(t, b, mode)
	insertDoc(t, b, "testdb", "items", seedItem(t, 1))
	insertDoc(t, b, "testdb", "items", seedItem(t, 2))
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "x")
	branchFrom(t, b, "testdb", "main", "y")
	s.x(t, b, "x")
	x1 := hash.Parse(commitBranch(t, b, "testdb", "x", "x1"))
	s.y(t, b, "y")
	y1 := hash.Parse(commitBranch(t, b, "testdb", "y", "y1"))

	db, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	virtualBase, flags, err := db.virtualMergeBase(context.Background(), []hash.Hash{y1, x1})
	require.NoError(t, err)
	return virtualBase, flags
}

// virtualBaseItems returns the documents of items in the commit |h|, by _id.
func virtualBaseItems(t *testing.T, b *Backend, h hash.Hash) map[int32]*types.Document {
	t.Helper()
	ctx := context.Background()
	db, err := b.getOrOpenDB(ctx, "testdb", false)
	require.NoError(t, err)
	am, err := amFromCommitHash(ctx, db, h.String())
	require.NoError(t, err)
	m, err := collectionMapFromAM(ctx, db, am, "items")
	require.NoError(t, err)
	iter, err := m.IterAll(ctx)
	require.NoError(t, err)
	docs := make(map[int32]*types.Document)
	for {
		_, v, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		doc, err := readDocFromValue(ctx, db.ns, v)
		require.NoError(t, err)
		id, err := doc.Get("_id")
		require.NoError(t, err)
		docs[id.(int32)] = doc
	}
	return docs
}

// flagsByID returns the flags of collection items by _id, with their paths sorted.
func flagsByID(flags virtualBaseFlags) map[int32]docFlag {
	byID := make(map[int32]docFlag)
	for _, flag := range flags["items"] {
		sort.Slice(flag.paths, func(i, j int) bool { return pathLess(flag.paths[i], flag.paths[j]) })
		byID[flag.id.(int32)] = docFlag{whole: flag.whole, paths: flag.paths}
	}
	return byID
}

func pathLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func requireSameDoc(t *testing.T, want, got *types.Document, msgAndArgs ...any) {
	t.Helper()
	if want == nil || got == nil {
		require.True(t, want == nil && got == nil, msgAndArgs...)
		return
	}
	require.True(t, documentsEqual(want, got), append([]any{"want %v, got %v"}, want, got)...)
}

func TestVirtualMergeBase_FoldsConflictingDocuments(t *testing.T) {
	for _, s := range foldScenarios(t) {
		for _, mode := range mergeModes {
			t.Run(s.name+"/"+mode, func(t *testing.T) {
				b := newTestBackend(t)
				virtualBase, flags := foldMergeBases(t, b, mode, s)
				docs := virtualBaseItems(t, b, virtualBase)
				want := s.want[mode]

				requireSameDoc(t, want.doc, docs[s.id])
				requireSameDoc(t, seedItem(t, 2), docs[2])

				wantFlags := map[int32]docFlag{}
				if want.flag != nil {
					wantFlags[s.id] = *want.flag
				}
				require.Equal(t, wantFlags, flagsByID(flags))
			})
		}
	}
}
