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
	"sort"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// The scenarios in this file fold two merge bases, x1 and y1, which edit c0:
//
//	c0 -- x1    x
//	  \-- y1    y
//
// y1 is committed after x1.

func foldEdits(t *testing.T, b *Backend, setup, x, y func(t *testing.T, b *Backend, branch string)) (hash.Hash, virtualBaseFlags) {
	t.Helper()
	setup(t, b, "main")
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "x")
	branchFrom(t, b, "testdb", "main", "y")
	x(t, b, "x")
	x1 := hash.Parse(commitBranch(t, b, "testdb", "x", "x1"))
	y(t, b, "y")
	y1 := hash.Parse(commitBranch(t, b, "testdb", "y", "y1"))

	db, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	virtualBase, flags, err := db.virtualMergeBase(context.Background(), []hash.Hash{y1, x1})
	require.NoError(t, err)
	return virtualBase, flags
}

func collModOn(t *testing.T, b *Backend, branch string, params *backends.CollModParams) {
	t.Helper()
	db, err := b.Database("testdb@" + branch)
	require.NoError(t, err)
	require.NoError(t, db.CollMod(context.Background(), params))
}

func createCollectionOn(t *testing.T, b *Backend, branch string, params *backends.CreateCollectionParams) {
	t.Helper()
	name := "testdb"
	if branch != "main" {
		name += "@" + branch
	}
	db, err := b.Database(name)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(context.Background(), params))
}

// virtualCatalog returns the metadata of collection |name| in the commit |h|, nil when it has none.
func virtualCatalog(t *testing.T, b *Backend, h hash.Hash, name string) *collMeta {
	t.Helper()
	ctx := context.Background()
	db, err := b.getOrOpenDB(ctx, "testdb", false)
	require.NoError(t, err)
	am, err := amFromCommitHash(ctx, db, h.String())
	require.NoError(t, err)
	meta, err := readCatalogDoc(ctx, db, am, name)
	require.NoError(t, err)
	return meta
}

// catalogFlagPaths returns the field paths the virtual merge base flags in the catalog document of |name|, sorted.
func catalogFlagPaths(t *testing.T, flags virtualBaseFlags, name string) [][]string {
	t.Helper()
	key, err := catalogKey(name)
	require.NoError(t, err)
	flag, ok := flags.docs[reservedCatalogName][string(key)]
	if !ok {
		return nil
	}
	require.False(t, flag.whole, "the catalog document of %q is flagged whole", name)
	paths := append([][]string(nil), flag.paths...)
	sort.Slice(paths, func(i, j int) bool { return pathLess(paths[i], paths[j]) })
	return paths
}

func requireExists(t *testing.T, field string) *types.Document {
	return mustDoc(t, field, mustDoc(t, "$exists", true))
}

func rDoc(t *testing.T, a string) *types.Document { return mustDoc(t, "_id", int32(1), "a", a) }

// Metadata the merge bases changed differently keeps its value in c0 and is flagged by field path, and no
// collection is deferred for a user resolution.
func TestVirtualMergeBase_FoldsConflictingValidators(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			b := newTestBackend(t)
			virtualBase, flags := foldEdits(t, b,
				func(t *testing.T, b *Backend, branch string) {
					createItems(t, b, mode)
					insertDoc(t, b, "testdb", "items", rDoc(t, "seed"))
				},
				func(t *testing.T, b *Backend, branch string) {
					collModOn(t, b, branch, &backends.CollModParams{Name: "items", Validator: requireExists(t, "a"), SetValidator: true})
				},
				func(t *testing.T, b *Backend, branch string) {
					collModOn(t, b, branch, &backends.CollModParams{Name: "items", Validator: requireExists(t, "_id"), SetValidator: true})
				})
			meta := virtualCatalog(t, b, virtualBase, "items")
			require.NotNil(t, meta)
			require.Nil(t, meta.Validator)
			require.Equal(t, [][]string{{"validator"}}, catalogFlagPaths(t, flags, "items"))
			requireSameDoc(t, rDoc(t, "seed"), virtualBaseItems(t, b, virtualBase)[1])
		})
	}
}

// H9: when the merge bases set merge modes that do not resolve three-way, the fold merges the collection's documents
// in their join. x1 and y1 also both set r.a to the same value, which tells the modes apart: documentTouched flags r
// whole, fieldTouched flags its path a, and the divergent modes merge it.
func TestVirtualMergeBase_MergeModeJoin(t *testing.T) {
	sameA := setItem(func(t *testing.T) *types.Document { return rDoc(t, "same") })
	tests := []struct {
		name           string
		c0, x1, y1     string
		wantDoc        func(t *testing.T) *types.Document
		wantFlag       *docFlag
		wantCatalogFlg bool
	}{
		{"fieldTouched and documentDivergent join in documentTouched", backends.MergeModeFieldDivergent,
			backends.MergeModeFieldTouched, backends.MergeModeDocumentDivergent,
			func(t *testing.T) *types.Document { return rDoc(t, "seed") }, wholeFlag(), true},
		{"documentTouched and fieldDivergent join in documentTouched", backends.MergeModeFieldTouched,
			backends.MergeModeDocumentTouched, backends.MergeModeFieldDivergent,
			func(t *testing.T) *types.Document { return rDoc(t, "seed") }, wholeFlag(), true},
		{"fieldTouched and fieldDivergent join in fieldTouched", backends.MergeModeDocumentDivergent,
			backends.MergeModeFieldTouched, backends.MergeModeFieldDivergent,
			func(t *testing.T) *types.Document { return rDoc(t, "seed") }, pathFlag([]string{"a"}), true},
		{"a mode one merge base set resolves three-way", backends.MergeModeFieldTouched,
			backends.MergeModeFieldDivergent, "",
			func(t *testing.T) *types.Document { return rDoc(t, "same") }, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestBackend(t)
			setMode := func(mode string) func(t *testing.T, b *Backend, branch string) {
				return func(t *testing.T, b *Backend, branch string) {
					if mode != "" {
						collModOn(t, b, branch, &backends.CollModParams{Name: "items", MergeMode: mode})
					}
					sameA(t, b, branch)
				}
			}
			virtualBase, flags := foldEdits(t, b,
				func(t *testing.T, b *Backend, branch string) {
					createItems(t, b, tt.c0)
					insertDoc(t, b, "testdb", "items", rDoc(t, "seed"))
				},
				setMode(tt.x1), setMode(tt.y1))
			requireSameDoc(t, tt.wantDoc(t), virtualBaseItems(t, b, virtualBase)[1])
			wantFlags := map[int32]docFlag{}
			if tt.wantFlag != nil {
				wantFlags[1] = *tt.wantFlag
			}
			require.Equal(t, wantFlags, flagsByID(flags))
			if tt.wantCatalogFlg {
				require.Equal(t, [][]string{{"mergeMode"}}, catalogFlagPaths(t, flags, "items"))
			} else {
				require.Nil(t, catalogFlagPaths(t, flags, "items"))
			}
		})
	}
}

// K4: both merge bases create collection fresh, one with a validator, and add the same document 100. The virtual
// merge base keeps fresh's catalog document with the fields they agree on and flags the validator. Document 100,
// added alike, is kept in the divergent modes and left absent and flagged in the touched modes.
func TestVirtualMergeBase_MergeBasesCreateTheSameCollection(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			b := newTestBackend(t)
			doc100 := func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(100), "v", int32(1)) }
			virtualBase, flags := foldEdits(t, b,
				func(t *testing.T, b *Backend, branch string) {
					createItems(t, b, mode)
				},
				func(t *testing.T, b *Backend, branch string) {
					createCollectionOn(t, b, branch, &backends.CreateCollectionParams{Name: "fresh", MergeMode: mode,
						Validator: requireExists(t, "v")})
					insertDoc(t, b, "testdb@"+branch, "fresh", doc100(t))
				},
				func(t *testing.T, b *Backend, branch string) {
					createCollectionOn(t, b, branch, &backends.CreateCollectionParams{Name: "fresh", MergeMode: mode})
					insertDoc(t, b, "testdb@"+branch, "fresh", doc100(t))
				})
			meta := virtualCatalog(t, b, virtualBase, "fresh")
			require.NotNil(t, meta)
			require.Nil(t, meta.Validator)
			require.NotEmpty(t, meta.UUID)
			require.Equal(t, [][]string{{"validator"}}, catalogFlagPaths(t, flags, "fresh"))

			ctx := context.Background()
			db, err := b.getOrOpenDB(ctx, "testdb", false)
			require.NoError(t, err)
			am, err := amFromCommitHash(ctx, db, virtualBase.String())
			require.NoError(t, err)
			freshDocs, err := collectionMapFromAM(ctx, db, am, "fresh")
			require.NoError(t, err)
			count, err := freshDocs.Count()
			require.NoError(t, err)
			touched := mode == backends.MergeModeDocumentTouched || mode == backends.MergeModeFieldTouched
			if touched {
				require.Equal(t, 0, count)
				require.Len(t, flags.docs["fresh"], 1)
			} else {
				require.Equal(t, 1, count)
				require.Empty(t, flags.docs["fresh"])
			}
		})
	}
}

// 6a: x1 drops items and y1 edits one of its documents. As for a document deleted on one side and modified on the
// other, the virtual merge base keeps items as in c0, catalog document included, unflagged.
func TestVirtualMergeBase_CollectionDroppedAndModified(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			b := newTestBackend(t)
			virtualBase, flags := foldEdits(t, b,
				func(t *testing.T, b *Backend, branch string) {
					createItems(t, b, mode)
					insertDoc(t, b, "testdb", "items", rDoc(t, "seed"))
				},
				func(t *testing.T, b *Backend, branch string) {
					db, err := b.Database("testdb@" + branch)
					require.NoError(t, err)
					require.NoError(t, db.DropCollection(context.Background(), &backends.DropCollectionParams{Name: "items"}))
				},
				setItem(func(t *testing.T) *types.Document { return rDoc(t, "y") }))
			require.NotNil(t, virtualCatalog(t, b, virtualBase, "items"))
			requireSameDoc(t, rDoc(t, "seed"), virtualBaseItems(t, b, virtualBase)[1])
			require.Empty(t, flags.docs["items"])
			require.Nil(t, catalogFlagPaths(t, flags, "items"))
		})
	}
}

func matchPipeline(t *testing.T, value string) *types.Array {
	t.Helper()
	pipeline, err := types.NewArray(mustDoc(t, "$match", mustDoc(t, "a", value)))
	require.NoError(t, err)
	return pipeline
}

// virtualView returns the definition of view |name| in the commit |h|, nil when there is none.
func virtualView(t *testing.T, b *Backend, h hash.Hash, name string) *viewMeta {
	t.Helper()
	ctx := context.Background()
	db, err := b.getOrOpenDB(ctx, "testdb", false)
	require.NoError(t, err)
	am, err := amFromCommitHash(ctx, db, h.String())
	require.NoError(t, err)
	viewH, err := am.Get(ctx, name)
	require.NoError(t, err)
	if viewH.IsEmpty() {
		return nil
	}
	isView, err := isViewEntry(ctx, db.cs, viewH)
	require.NoError(t, err)
	require.True(t, isView)
	vm, err := readViewChunk(ctx, db.ns, viewH)
	require.NoError(t, err)
	return vm
}

// A view the merge bases define differently keeps its definition in c0, or is left absent when both added it, and
// is flagged.
func TestVirtualMergeBase_FoldsConflictingViews(t *testing.T) {
	createView := func(value string) func(t *testing.T, b *Backend, branch string) {
		return func(t *testing.T, b *Backend, branch string) {
			createCollectionOn(t, b, branch, &backends.CreateCollectionParams{Name: "v", ViewOn: "items", ViewPipeline: matchPipeline(t, value)})
		}
	}
	changeView := func(value string) func(t *testing.T, b *Backend, branch string) {
		return func(t *testing.T, b *Backend, branch string) {
			collModOn(t, b, branch, &backends.CollModParams{Name: "v", SetView: true, ViewPipeline: matchPipeline(t, value), SetViewPipeline: true})
		}
	}
	setup := func(withView bool) func(t *testing.T, b *Backend, branch string) {
		return func(t *testing.T, b *Backend, branch string) {
			createItems(t, b, "")
			insertDoc(t, b, "testdb", "items", rDoc(t, "seed"))
			if withView {
				createView("c0")(t, b, branch)
			}
		}
	}

	t.Run("added differently", func(t *testing.T) {
		b := newTestBackend(t)
		virtualBase, flags := foldEdits(t, b, setup(false), createView("x"), createView("y"))
		require.Nil(t, virtualView(t, b, virtualBase, "v"))
		require.Equal(t, map[string]bool{"v": true}, flags.views)
	})
	t.Run("changed differently", func(t *testing.T) {
		b := newTestBackend(t)
		virtualBase, flags := foldEdits(t, b, setup(true), changeView("x"), changeView("y"))
		vm := virtualView(t, b, virtualBase, "v")
		require.NotNil(t, vm)
		require.True(t, reflectEqual(matchPipeline(t, "c0"), vm.Pipeline), "want c0's pipeline, got %v", vm.Pipeline)
		require.Equal(t, map[string]bool{"v": true}, flags.views)
	})
}

func reflectEqual(want, got *types.Array) bool {
	return fieldsEqual(mustDocOf(want), mustDocOf(got), "p")
}

func mustDocOf(array *types.Array) *types.Document {
	return must1(types.NewDocument("p", array))
}

func must1(doc *types.Document, err error) *types.Document {
	if err != nil {
		panic(err)
	}
	return doc
}
