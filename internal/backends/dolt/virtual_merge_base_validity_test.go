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
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// validityScenario folds two merge bases, x1 and y1, which edit c0 with |x| and |y|:
//
//	c0 -- x1    x
//	  \-- y1    y
//
// |setup| creates items in c0. want is every document of the virtual merge base and its flag, nil when unflagged,
// by merge mode. y1 is committed after x1, so it is folded after it.
type validityScenario struct {
	name  string
	setup func(t *testing.T, b *Backend, mode string)
	x, y  func(t *testing.T, b *Backend, branch string)
	want  func(t *testing.T, mode string) map[int32]foldResult
}

func uDoc(t *testing.T, id, u int32) *types.Document { return mustDoc(t, "_id", id, "u", u) }

func createUniqueItems(t *testing.T, b *Backend, mode string, docs ...*types.Document) {
	t.Helper()
	createItems(t, b, mode)
	createUniqueIndex(t, b, "main")
	for _, doc := range docs {
		insertDoc(t, b, "testdb", "items", doc)
	}
}

func createUniqueIndex(t *testing.T, b *Backend, branch string) {
	t.Helper()
	_, err := collAt(t, b, "testdb", branch, "items").CreateIndexes(context.Background(), &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{Name: "by_u", Unique: true, Key: []backends.IndexKeyPair{{Field: "u"}}}},
	})
	require.NoError(t, err)
}

func createValidatedItems(t *testing.T, b *Backend, mode, action string, validator *types.Document, docs ...*types.Document) {
	t.Helper()
	db, err := b.Database("testdb")
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(context.Background(), &backends.CreateCollectionParams{
		Name: "items", MergeMode: mode, Validator: validator, ValidationAction: action,
	}))
	for _, doc := range docs {
		insertDoc(t, b, "testdb", "items", doc)
	}
}

// notBothXAndY is a validator that refuses a document with both fields x and y.
func notBothXAndY(t *testing.T) *types.Document {
	both := mustDoc(t, "x", mustDoc(t, "$exists", true), "y", mustDoc(t, "$exists", true))
	array, err := types.NewArray(both)
	require.NoError(t, err)
	return mustDoc(t, "$nor", array)
}

func sameForEveryMode(want map[int32]foldResult) func(t *testing.T, mode string) map[int32]foldResult {
	return func(*testing.T, string) map[int32]foldResult { return want }
}

func validityScenarios() []validityScenario {
	return []validityScenario{
		{
			// I2a: x1 and y1 each give a different document u = 5. The document y1 changed loses the unique key and
			// keeps its value in c0.
			name: "unique collision",
			setup: func(t *testing.T, b *Backend, mode string) {
				createUniqueItems(t, b, mode, uDoc(t, 1, 1), uDoc(t, 2, 2))
			},
			x: setItem(func(t *testing.T) *types.Document { return uDoc(t, 1, 5) }),
			y: setItem(func(t *testing.T) *types.Document { return uDoc(t, 2, 5) }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{1: {uDoc(t, 1, 5), nil}, 2: {uDoc(t, 2, 2), wholeFlag()}}
			},
		},
		{
			// I2d: x1 sets A.u = 2 and B.u = 3, y1 sets B.u = 4. B conflicts and keeps c0's u = 2, which A now holds,
			// so B is left absent.
			name: "unique collision with a document's base value",
			setup: func(t *testing.T, b *Backend, mode string) {
				createUniqueItems(t, b, mode, uDoc(t, 1, 1), uDoc(t, 2, 2))
			},
			x: func(t *testing.T, b *Backend, branch string) {
				updateDocOnBranch(t, b, "testdb", branch, "items", uDoc(t, 2, 3))
				updateDocOnBranch(t, b, "testdb", branch, "items", uDoc(t, 1, 2))
			},
			y: setItem(func(t *testing.T) *types.Document { return uDoc(t, 2, 4) }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{1: {uDoc(t, 1, 2), nil}, 2: {nil, wholeFlag()}}
			},
		},
		{
			// I2e: x1 adds a unique index on u, and y1 gives document 2 the u that document 1 has.
			name: "unique index added by one merge base",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", uDoc(t, 1, 1))
				insertDoc(t, b, "testdb", "items", uDoc(t, 2, 2))
			},
			x: func(t *testing.T, b *Backend, branch string) { createUniqueIndex(t, b, branch) },
			y: setItem(func(t *testing.T) *types.Document { return uDoc(t, 2, 1) }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{1: {uDoc(t, 1, 1), nil}, 2: {uDoc(t, 2, 2), wholeFlag()}}
			},
		},
		{
			// I3a: x1 sets x and y1 sets y on document 1, which the validator refuses together.
			name: "merged document fails the validator",
			setup: func(t *testing.T, b *Backend, mode string) {
				createValidatedItems(t, b, mode, "", notBothXAndY(t), mustDoc(t, "_id", int32(1), "c", "seed"))
			},
			x: setItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(1), "c", "seed", "x", int32(1)) }),
			y: setItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(1), "c", "seed", "y", int32(1)) }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{1: {mustDoc(t, "_id", int32(1), "c", "seed"), wholeFlag()}}
			},
		},
		{
			// I3e: as above with validationAction "warn": the field modes keep the merged document. The document modes
			// conflict on document 1 regardless.
			name: "merged document fails a warning validator",
			setup: func(t *testing.T, b *Backend, mode string) {
				createValidatedItems(t, b, mode, "warn", notBothXAndY(t), mustDoc(t, "_id", int32(1), "c", "seed"))
			},
			x: setItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(1), "c", "seed", "x", int32(1)) }),
			y: setItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(1), "c", "seed", "y", int32(1)) }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				seed := mustDoc(t, "_id", int32(1), "c", "seed")
				merged := mustDoc(t, "_id", int32(1), "c", "seed", "x", int32(1), "y", int32(1))
				if mode == backends.MergeModeDocumentTouched || mode == backends.MergeModeDocumentDivergent {
					return map[int32]foldResult{1: {seed, wholeFlag()}}
				}
				return map[int32]foldResult{1: {merged, nil}}
			},
		},
		{
			// I3c: document 1 predates the validator and fails it. Neither merge base changes it, so it is not
			// validated.
			name: "grandfathered document",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "x", int32(1), "y", int32(1)))
				insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(2), "c", "seed"))
				db, err := b.Database("testdb")
				require.NoError(t, err)
				require.NoError(t, db.CollMod(context.Background(), &backends.CollModParams{
					Name: "items", Validator: notBothXAndY(t), SetValidator: true,
				}))
			},
			x: setItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(2), "c", "x") }),
			y: addItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(3), "c", "y") }),
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{
					1: {mustDoc(t, "_id", int32(1), "x", int32(1), "y", int32(1)), nil},
					2: {mustDoc(t, "_id", int32(2), "c", "x"), nil},
					3: {mustDoc(t, "_id", int32(3), "c", "y"), nil},
				}
			},
		},
		{
			// I3d: y1 adds a validator, and x1 adds document 9, which fails it.
			name: "validator added by one merge base",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "c", "seed"))
			},
			x: addItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(9), "x", int32(1), "y", int32(1)) }),
			y: func(t *testing.T, b *Backend, branch string) {
				db, err := b.Database("testdb@" + branch)
				require.NoError(t, err)
				require.NoError(t, db.CollMod(context.Background(), &backends.CollModParams{
					Name: "items", Validator: notBothXAndY(t), SetValidator: true,
				}))
			},
			want: func(t *testing.T, mode string) map[int32]foldResult {
				return map[int32]foldResult{1: {mustDoc(t, "_id", int32(1), "c", "seed"), nil}, 9: {nil, wholeFlag()}}
			},
		},
	}
}

func foldValidity(t *testing.T, b *Backend, mode string, s validityScenario) (hash.Hash, virtualBaseFlags) {
	t.Helper()
	s.setup(t, b, mode)
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

// requireIndexesMatchItems checks that each index of collection items in the commit |h| holds exactly the entries its
// documents give, and that no unique index holds a key twice: rule 4 for a virtual merge base.
func requireIndexesMatchItems(t *testing.T, b *Backend, h hash.Hash) {
	t.Helper()
	ctx := context.Background()
	db, err := b.getOrOpenDB(ctx, "testdb", false)
	require.NoError(t, err)
	am, err := amFromCommitHash(ctx, db, h.String())
	require.NoError(t, err)
	collH, err := am.Get(ctx, "items")
	require.NoError(t, err)
	idxAM, err := indexAMForDTBL(ctx, db.cs, db.ns, collH)
	require.NoError(t, err)
	indexes, err := indexSetFromAM(ctx, db, idxAM)
	require.NoError(t, err)
	docs, err := collectionMapFromAM(ctx, db, am, "items")
	require.NoError(t, err)

	for name, entry := range indexes {
		want := make(map[string]bool)
		uniqueKeys := make(map[string]bool)
		iter, err := docs.IterAll(ctx)
		require.NoError(t, err)
		for {
			k, v, err := iter.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			idBytes, ok := keyDesc.GetBytes(0, k)
			require.True(t, ok)
			doc, err := readDocFromValue(ctx, db.ns, v)
			require.NoError(t, err)
			rows, _, _ := indexEntriesForDoc(doc, entry.info)
			for _, row := range rows {
				key, _, err := idxpkg.BuildIndexEntry(row, idBytes)
				require.NoError(t, err)
				want[string(key)] = true
				if entry.info.Unique {
					prefix, _ := idxpkg.EqualityProbeBounds(row)
					require.False(t, uniqueKeys[string(prefix)], "unique index %q holds a key twice", name)
					uniqueKeys[string(prefix)] = true
				}
			}
		}
		m, err := openIndexMap(ctx, db.vs, db.ns, entry.mapRoot)
		require.NoError(t, err)
		got := make(map[string]bool)
		idxIter, err := m.IterAll(ctx)
		require.NoError(t, err)
		for {
			k, _, err := idxIter.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			got[string(k)] = true
		}
		require.Equal(t, want, got, "index %q does not match the documents", name)
	}
}

func TestVirtualMergeBase_StaysValid(t *testing.T) {
	for _, s := range validityScenarios() {
		for _, mode := range mergeModes {
			t.Run(s.name+"/"+mode, func(t *testing.T) {
				b := newTestBackend(t)
				virtualBase, flags := foldValidity(t, b, mode, s)
				docs := virtualBaseItems(t, b, virtualBase)
				want := s.want(t, mode)

				wantFlags := map[int32]docFlag{}
				for id, result := range want {
					requireSameDoc(t, result.doc, docs[id], "document %d", id)
					if result.flag != nil {
						wantFlags[id] = *result.flag
					}
				}
				for id := range docs {
					_, expected := want[id]
					require.True(t, expected, "unexpected document %d", id)
				}
				require.Equal(t, wantFlags, flagsByID(flags))
				requireIndexesMatchItems(t, b, virtualBase)
			})
		}
	}
}
