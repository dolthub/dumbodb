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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// edit changes items on a branch.
type edit func(t *testing.T, b *Backend, branch string)

func editAll(edits ...edit) edit {
	return func(t *testing.T, b *Backend, branch string) {
		for _, e := range edits {
			e(t, b, branch)
		}
	}
}

// mergeResolving merges |from| into |into|, resolving any conflicts with ours, then applies |resolve| and commits it.
func mergeResolving(t *testing.T, b *Backend, into, from string, resolve edit) {
	t.Helper()
	ctx := context.Background()
	_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "testdb", Into: into, From: from, Message: "merge " + from, Author: "testuser <test@example.com>",
	})
	if err != nil {
		res, cerr := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "testdb", Branch: into})
		require.NoError(t, cerr, "merge of %s into %s: %v", from, into, err)
		for _, cc := range res.Collections {
			for _, c := range cc.Conflicts {
				require.NoError(t, resolveConflict(t, b, "testdb", into, cc.Collection, c.ConflictID, "ours", nil))
			}
		}
		continueMerge(t, b, "testdb", into)
	}
	if resolve != nil {
		resolve(t, b, into)
		// The resolution may already be in place, as when ours deleted the document too.
		_, err := b.DumboDBCommit(ctx, &backends.CommitParams{
			DBName: "testdb", Branch: into, Message: "resolve merge of " + from, Author: "testuser", AllowEmpty: true,
		})
		require.NoError(t, err)
	}
}

// outerResult is the outcome of merging two branches with two merge bases: a conflict on the document, or a clean
// merge leaving |doc|, nil when the document is absent.
type outerResult struct {
	conflict bool
	doc      *types.Document
}

func conflicts() outerResult { return outerResult{conflict: true} }

func merges(doc *types.Document) outerResult { return outerResult{doc: doc} }

// outerScenario is a criss-cross whose merge bases, x1 and y1, edit c0 with |x1| and |y1|:
//
//	c0 -- x1 -- x2 [-- x3]    x
//	  \-- y1 -- y2 [-- y3]    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)
//
// x2 and y2 resolve their merges with ours and then apply |x2| and |y2|; x3 and y3, when set, edit further. c0 has
// documents 1 and 2 of seedItem in items. want is the outcome for document |id| of merging y into x, or x into y,
// by merge mode; both directions give the same document.
type outerScenario struct {
	name   string
	id     int32
	x1, y1 edit
	x2, y2 edit
	x3, y3 edit
	want   func(t *testing.T) map[string]outerResult
}

func setTo(build func(t *testing.T) *types.Document) edit { return edit(setItem(build)) }

func itemA(a string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return itemDoc(t, 1, a, "seed", "seed", "seed", 1) }
}

func itemAB(a, b string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return itemDoc(t, 1, a, b, "seed", "seed", 1) }
}

func doc3(a string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return itemDoc(t, 3, a, "seed", "seed", "seed", 1) }
}

func outcomes(dT, fT, fD, dD outerResult) map[string]outerResult {
	return map[string]outerResult{
		"documentTouched": dT, "fieldTouched": fT, "fieldDivergent": fD, "documentDivergent": dD,
	}
}

func outerScenarios() []outerScenario {
	return []outerScenario{
		{
			// B1: both tips resolve a alike. The touched modes refuse two writers of a.
			name: "tips resolve a conflict between the merge bases alike",
			id:   1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("resolved")), y2: setTo(itemA("resolved")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(itemA("resolved")(t)), merges(itemA("resolved")(t)))
			},
		},
		{
			// B2
			name: "tips resolve a conflict between the merge bases differently",
			id:   1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("x wins")), y2: setTo(itemA("y wins")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
		{
			// B4 (Dolt 4e): x2 resolves back to c0's value, which is still a change from the conflicted merge bases.
			name: "one tip resolves a conflict back to the old value",
			id:   1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("seed")), y2: setTo(itemA("y")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
		{
			// Both tips resolve back to c0's value, the virtual merge base's own: the differ sees no change, but both
			// tips wrote a.
			name: "both tips resolve a conflict back to the old value",
			id:   1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("seed")), y2: setTo(itemA("seed")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(itemA("seed")(t)), merges(itemA("seed")(t)))
			},
		},
		{
			// B6
			name: "tips resolve alike, then one edits another field",
			id:   1, x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("resolved")), y2: setTo(itemA("resolved")),
			y3: setTo(itemAB("resolved", "later")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(itemAB("resolved", "later")(t)), conflicts())
			},
		},
		{
			// B10: the merge bases edit different fields, which only the document modes refuse between them.
			name: "merge bases edit different fields, tips agree",
			id:   1, x1: setTo(itemAB("x", "seed")), y1: setTo(itemAB("seed", "y")),
			x2: setTo(itemAB("x", "y")), y2: setTo(itemAB("x", "y")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), merges(itemAB("x", "y")(t)), merges(itemAB("x", "y")(t)), merges(itemAB("x", "y")(t)))
			},
		},
		{
			// C1: a delete against a modify keeps c0's document unflagged; both tips delete it.
			name: "merge bases delete against modify, both tips delete",
			id:   1, x1: deleteItem(1), y1: setTo(itemA("y")), x2: deleteItem(1), y2: deleteItem(1),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(nil), merges(nil))
			},
		},
		{
			// C3
			name: "merge bases delete against modify, one tip deletes",
			id:   1, x1: deleteItem(1), y1: setTo(itemA("y")), x2: deleteItem(1), y2: setTo(itemA("y")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
		{
			// D1
			name: "merge bases add a document differently, tips resolve it alike",
			id:   3, x1: addItem(doc3("x")), y1: addItem(doc3("y")), x2: setTo(doc3("same")), y2: setTo(doc3("same")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(doc3("same")(t)), merges(doc3("same")(t)))
			},
		},
		{
			// D3: the virtual merge base has document 3 absent and flagged, so x2's delete is a change too.
			name: "merge bases add a document differently, one tip deletes it",
			id:   3, x1: addItem(doc3("x")), y1: addItem(doc3("y")), x2: deleteItem(3), y2: setTo(doc3("y")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
		{
			// D4: both tips delete a document the merge bases added differently, the "both sides deleted" rule.
			name: "merge bases add a document differently, both tips delete it",
			id:   3, x1: addItem(doc3("x")), y1: addItem(doc3("y")), x2: deleteItem(3), y2: deleteItem(3),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), merges(nil), merges(nil))
			},
		},
	}
}

func buildOuterScenario(t *testing.T, b *Backend, mode string, s outerScenario) {
	t.Helper()
	createItems(t, b, mode)
	insertDoc(t, b, "testdb", "items", seedItem(t, 1))
	insertDoc(t, b, "testdb", "items", seedItem(t, 2))
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "x")
	branchFrom(t, b, "testdb", "main", "y")
	s.x1(t, b, "x")
	commitBranch(t, b, "testdb", "x", "x1")
	branchFrom(t, b, "testdb", "x", "x1")
	s.y1(t, b, "y")
	commitBranch(t, b, "testdb", "y", "y1")
	branchFrom(t, b, "testdb", "y", "y1")
	mergeResolving(t, b, "x", "y1", s.x2)
	mergeResolving(t, b, "y", "x1", s.y2)
	if s.x3 != nil {
		s.x3(t, b, "x")
		commitBranch(t, b, "testdb", "x", "x3")
	}
	if s.y3 != nil {
		s.y3(t, b, "y")
		commitBranch(t, b, "testdb", "y", "y3")
	}
	requireMergeBases(t, b, "x", "y", "y1", "x1")
}

// requireOuterResult merges |from| into |into| and checks the outcome for document |id|.
func requireOuterResult(t *testing.T, b *Backend, into, from string, id int32, want outerResult) {
	t.Helper()
	ctx := context.Background()
	_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "testdb", Into: into, From: from, Message: "merge " + from, Author: "testuser <test@example.com>",
	})
	if want.conflict {
		require.Error(t, err, "merging %s into %s must conflict", from, into)
		res, cerr := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "testdb", Branch: into})
		require.NoError(t, cerr)
		var ids []any
		for _, cc := range res.Collections {
			for _, c := range cc.Conflicts {
				for _, doc := range []*types.Document{c.Ours, c.Theirs, c.Base} {
					if doc != nil {
						v, _ := doc.Get("_id")
						ids = append(ids, v)
						break
					}
				}
			}
		}
		require.Contains(t, ids, id, "merging %s into %s must conflict on document %d", from, into, id)
		return
	}
	require.NoError(t, err, "merging %s into %s must be clean", from, into)
	docs := virtualBaseItems(t, b, branchHash(t, b, into))
	requireSameDoc(t, want.doc, docs[id], "document %d after merging %s into %s", id, from, into)
}

func TestVirtualMergeBase_OuterMergeHonorsFlags(t *testing.T) {
	for _, s := range outerScenarios() {
		for _, mode := range mergeModes {
			t.Run(s.name+"/"+mode+"/merge y into x", func(t *testing.T) {
				b := newTestBackend(t)
				buildOuterScenario(t, b, mode, s)
				requireOuterResult(t, b, "x", "y", s.id, s.want(t)[mode])
			})
			t.Run(s.name+"/"+mode+"/merge x into y", func(t *testing.T) {
				b := newTestBackend(t)
				buildOuterScenario(t, b, mode, s)
				requireOuterResult(t, b, "y", "x", s.id, s.want(t)[mode])
			})
		}
	}
}

// F1 (Dolt 6c): two criss-crosses, one on top of the other:
//
//	c0 -- x1 -- x2 -- x3    x
//	  \-- y1 -- y2 -- y3    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)
//	x3 = merge(x2, y2), y3 = merge(y2, x2)
//
// x1 and y1 set a to x and y. x2 resolves it back to c0's value and y2 to y, so x2 and y2 conflict too, which needs
// the flag from their own virtual merge base. x3 resolves it to y and y3 to q. The merge bases of x3 and y3 are x2
// and y2, whose own merge bases are x1 and y1, so the virtual merge base of x2 and y2 must honor the flags of the
// virtual merge base of x1 and y1.
func TestVirtualMergeBase_NestedCrissCross(t *testing.T) {
	for _, mode := range mergeModes {
		for _, direction := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
			t.Run(mode+"/merge "+direction.from+" into "+direction.into, func(t *testing.T) {
				b := newTestBackend(t)
				s := outerScenario{x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("seed")), y2: setTo(itemA("y"))}
				buildOuterScenario(t, b, mode, s)
				branchFrom(t, b, "testdb", "x", "x2")
				branchFrom(t, b, "testdb", "y", "y2")
				mergeResolving(t, b, "x", "y2", setTo(itemA("y")))
				mergeResolving(t, b, "y", "x2", setTo(itemA("q")))
				requireMergeBases(t, b, "x", "y", "y2", "x2")
				requireOuterResult(t, b, direction.into, direction.from, 1, conflicts())
			})
		}
	}
}
