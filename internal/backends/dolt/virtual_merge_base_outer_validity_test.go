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
	"github.com/dolthub/dumbodb/internal/types"
)

// Section I: outerScenario histories whose merge bases x1 and y1 collide on a unique index or fail a validator, so
// that the virtual merge base reverts or drops a document to stay valid, and flags it.

func uItem(id, u int32) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return uDoc(t, id, u) }
}

// setUs sets each document in order, adding it when absent, so that no step collides on the unique index.
func setUs(docs ...func(t *testing.T) *types.Document) edit {
	edits := make([]edit, len(docs))
	for i, doc := range docs {
		edits[i] = upsertItem(doc)
	}
	return editAll(edits...)
}

func addValidator(t *testing.T, b *Backend, branch string) {
	t.Helper()
	db, err := b.Database("testdb@" + branch)
	require.NoError(t, err)
	require.NoError(t, db.CollMod(context.Background(), &backends.CollModParams{
		Name: "items", Validator: notBothXAndY(t), SetValidator: true,
	}))
}

func outerValidityScenarios() []outerScenario {
	uniqueItems := func(docs ...func(t *testing.T) *types.Document) func(t *testing.T, b *Backend, mode string) {
		return func(t *testing.T, b *Backend, mode string) {
			built := make([]*types.Document, len(docs))
			for i, doc := range docs {
				built[i] = doc(t)
			}
			createUniqueItems(t, b, mode, built...)
		}
	}
	validatedItems := func(action string) func(t *testing.T, b *Backend, mode string) {
		return func(t *testing.T, b *Backend, mode string) {
			createValidatedItems(t, b, mode, action, notBothXAndY(t), xyDoc()(t))
		}
	}
	indexedItems := func(t *testing.T, b *Backend, mode string) {
		createItems(t, b, mode)
		createIndex(t, context.Background(), collAt(t, b, "testdb", "main", "items"), "by_b", "b")
		insertDoc(t, b, "testdb", "items", seedItem(t, 1))
		insertDoc(t, b, "testdb", "items", seedItem(t, 2))
	}
	same := func(r func(t *testing.T) outerResult) func(t *testing.T) map[string]outerResult {
		return func(t *testing.T) map[string]outerResult { return outcomes(r(t), r(t), r(t), r(t)) }
	}
	touchedConflict := func(doc func(t *testing.T) *types.Document) func(t *testing.T) map[string]outerResult {
		return func(t *testing.T) map[string]outerResult {
			return outcomes(conflicts(), conflicts(), merges(doc(t)), merges(doc(t)))
		}
	}
	allConflict := same(func(*testing.T) outerResult { return conflicts() })
	i2a := outerScenario{
		setup: uniqueItems(uItem(1, 1), uItem(2, 2)),
		x1:    setTo(uItem(1, 5)), y1: setTo(uItem(2, 5)),
		x2: setUs(uItem(2, 6), uItem(1, 5)), y2: setUs(uItem(2, 6), uItem(1, 5)),
	}
	with := func(s outerScenario, name string, id int32, change func(s *outerScenario), want func(t *testing.T) map[string]outerResult) outerScenario {
		s.name, s.id, s.want = name, id, want
		if change != nil {
			change(&s)
		}
		return s
	}
	return []outerScenario{
		{
			// I1 (Dolt 5c): B1 with an index on b, then x3 edits document 2.
			name: "B1 with an index", setup: indexedItems,
			id: 1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("resolved")), y2: setTo(itemA("resolved")),
			x3:   setTo(func(t *testing.T) *types.Document { return itemDoc(t, 2, "seed", "x3", "seed", "seed", 1) }),
			want: touchedConflict(itemA("resolved")),
		},
		{
			name: "B1 with an index, the other document", setup: indexedItems,
			id: 2, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("resolved")), y2: setTo(itemA("resolved")),
			x3: setTo(func(t *testing.T) *types.Document { return itemDoc(t, 2, "seed", "x3", "seed", "seed", 1) }),
			want: same(func(t *testing.T) outerResult {
				return mergesDespiteOthers(itemDoc(t, 2, "seed", "x3", "seed", "seed", 1))
			}),
		},
		// I2a: x1 sets A.u = 5 and y1 B.u = 5. The virtual merge base keeps B's u = 2 and flags B; the tips both set
		// B.u = 6.
		with(i2a, "unique collision, tips resolve alike", 2, nil, touchedConflict(uItem(2, 6))),
		with(i2a, "unique collision, the document that kept the key", 1, nil,
			same(func(t *testing.T) outerResult { return mergesDespiteOthers(uDoc(t, 1, 5)) })),
		// I2b
		with(i2a, "unique collision, tips resolve differently", 2,
			func(s *outerScenario) { s.y2 = setUs(uItem(2, 7), uItem(1, 5)) }, allConflict),
		// I2c: x3 edits document 3, which nothing flags.
		with(i2a, "unique collision, a tip edits another document", 3,
			func(s *outerScenario) {
				s.setup = uniqueItems(uItem(1, 1), uItem(2, 2), uItem(3, 3))
				s.x3 = setTo(uItem(3, 30))
			},
			same(func(t *testing.T) outerResult { return mergesDespiteOthers(uDoc(t, 3, 30)) })),
		{
			// I2d: x1 sets A.u = 2 and B.u = 3, y1 sets B.u = 4. B keeps c0's u = 2, which A holds, so the virtual
			// merge base leaves B absent; the tips both set B.u = 3.
			name: "unique collision with a document's base value", setup: uniqueItems(uItem(1, 1), uItem(2, 2)),
			id: 2, x1: setUs(uItem(2, 3), uItem(1, 2)), y1: setTo(uItem(2, 4)),
			x2: setUs(uItem(1, 2), uItem(2, 3)), y2: setUs(uItem(2, 3), uItem(1, 2)),
			want: touchedConflict(uItem(2, 3)),
		},
		{
			// I2e: x1 adds a unique index on u, and y1 gives document 2 the u of document 1. The tips both set
			// document 2's u to 3.
			name: "unique index added by one merge base",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", uDoc(t, 1, 1))
				insertDoc(t, b, "testdb", "items", uDoc(t, 2, 2))
			},
			id: 2, x1: createUniqueIndex, y1: setTo(uItem(2, 1)), x2: upsertItem(uItem(2, 3)), y2: upsertItem(uItem(2, 3)),
			want: touchedConflict(uItem(2, 3)),
		},
		{
			// I3a: x1 sets x and y1 sets y on document 1, which the validator refuses together; the tips both keep
			// only x.
			name: "merged document fails the validator, tips agree", setup: validatedItems(""),
			id: 1, x1: setTo(xyDoc("x", int32(1))), y1: setTo(xyDoc("y", int32(1))),
			x2: upsertItem(xyDoc("x", int32(1))), y2: upsertItem(xyDoc("x", int32(1))),
			want: touchedConflict(xyDoc("x", int32(1))),
		},
		{
			// I3b
			name: "merged document fails the validator, tips disagree", setup: validatedItems(""),
			id: 1, x1: setTo(xyDoc("x", int32(1))), y1: setTo(xyDoc("y", int32(1))),
			x2: upsertItem(xyDoc("x", int32(1))), y2: upsertItem(xyDoc("x", int32(2))),
			want: allConflict,
		},
		{
			// I3c: document 1 predates the validator and fails it. The merge bases and tips leave it alone.
			name: "grandfathered document",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "x", int32(1), "y", int32(1)))
				insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(2), "c", "seed"))
				addValidator(t, b, "main")
			},
			id: 1, x1: setTo(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(2), "c", "x") }),
			y1: addItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(3), "c", "y") }),
			x2: editAll(), y2: editAll(),
			want: same(func(t *testing.T) outerResult {
				return merges(mustDoc(t, "_id", int32(1), "x", int32(1), "y", int32(1)))
			}),
		},
		{
			// I3d: y1 adds the validator, and x1 adds document 9, which fails it. The virtual merge base leaves
			// document 9 absent; the tips both set it to a valid document.
			name: "validator added by one merge base",
			setup: func(t *testing.T, b *Backend, mode string) {
				createItems(t, b, mode)
				insertDoc(t, b, "testdb", "items", xyDoc()(t))
			},
			id: 9, x1: addItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(9), "x", int32(1), "y", int32(1)) }),
			y1:   addValidator,
			x2:   upsertItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(9), "x", int32(1)) }),
			y2:   upsertItem(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(9), "x", int32(1)) }),
			want: touchedConflict(func(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(9), "x", int32(1)) }),
		},
		{
			// I3e: I3a with validationAction "warn". The field modes keep the merged document unflagged, and the tips
			// both keep it too.
			name: "merged document fails a warning validator, tips agree", setup: validatedItems("warn"),
			id: 1, x1: setTo(xyDoc("x", int32(1))), y1: setTo(xyDoc("y", int32(1))),
			x2: upsertItem(xyDoc("x", int32(1), "y", int32(1))), y2: upsertItem(xyDoc("x", int32(1), "y", int32(1))),
			want: func(t *testing.T) map[string]outerResult {
				kept := merges(xyDoc("x", int32(1), "y", int32(1))(t))
				return outcomes(conflicts(), kept, kept, kept)
			},
		},
	}
}

// upsertItem sets a document, adding it when absent.
func upsertItem(doc func(t *testing.T) *types.Document) edit {
	return func(t *testing.T, b *Backend, branch string) {
		_, err := collAt(t, b, "testdb", branch, "items").DeleteAll(context.Background(), &backends.DeleteAllParams{
			IDs: []any{mustID(t, doc(t))},
		})
		require.NoError(t, err)
		insertDoc(t, b, "testdb@"+branch, "items", doc(t))
	}
}

func mustID(t *testing.T, doc *types.Document) any {
	id, err := doc.Get("_id")
	require.NoError(t, err)
	return id
}

func TestVirtualMergeBase_OuterMergeHonorsValidity(t *testing.T) {
	for _, s := range outerValidityScenarios() {
		for _, mode := range mergeModes {
			for _, d := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
				t.Run(s.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					b := newTestBackend(t)
					buildOuterScenario(t, b, mode, s)
					requireOuterResult(t, b, d.into, d.from, s.id, s.want(t)[mode])
				})
			}
		}
	}
}

// I2f: three merge bases, as in section E, each giving a different document u = 9 on a unique index. The first fold
// keeps p1's A and flags B; the second keeps A and flags C. The tips both set B.u = 6 and C.u = 7.
func TestVirtualMergeBase_UniqueCollisionsAcrossThreeMergeBases(t *testing.T) {
	tips := setUs(uItem(2, 6), uItem(3, 7), uItem(1, 9))
	want := map[int32]func(t *testing.T) map[string]outerResult{
		1: func(t *testing.T) map[string]outerResult {
			kept := mergesDespiteOthers(uDoc(t, 1, 9))
			return outcomes(kept, kept, kept, kept)
		},
		2: func(t *testing.T) map[string]outerResult {
			return outcomes(conflicts(), conflicts(), merges(uDoc(t, 2, 6)), merges(uDoc(t, 2, 6)))
		},
		3: func(t *testing.T) map[string]outerResult {
			return outcomes(conflicts(), conflicts(), merges(uDoc(t, 3, 7)), merges(uDoc(t, 3, 7)))
		},
	}
	for _, id := range []int32{1, 2, 3} {
		for _, mode := range mergeModes {
			for _, d := range []struct{ into, from string }{{"m1", "m2"}, {"m2", "m1"}} {
				t.Run(fmt.Sprintf("document %d/%s/merge %s into %s", id, mode, d.from, d.into), func(t *testing.T) {
					useTestCommitClock(t)
					b := newTestBackend(t)
					setup := func(t *testing.T, b *Backend, mode string) {
						createUniqueItems(t, b, mode, uDoc(t, 1, 1), uDoc(t, 2, 2), uDoc(t, 3, 3))
					}
					threeBasesHistory(t, b, mode, setup,
						setTo(uItem(1, 9)), setTo(uItem(2, 9)), setTo(uItem(3, 9)),
						nil, nil, tips, tips)
					requireOuterResult(t, b, d.into, d.from, id, want[id](t)[mode])
				})
			}
		}
	}
}
