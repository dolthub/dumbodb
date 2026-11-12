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

// The scenarios in this file are criss-crosses whose merge bases, x1 and y1, change the validator of items or the
// definition of view v:
//
//	c0 -- x1 -- x2    x
//	  \-- y1 -- y2    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)
//
// x2 and y2 resolve their merges with ours and then apply their own edits.

func setValidator(validator func(t *testing.T) *types.Document) edit {
	return func(t *testing.T, b *Backend, branch string) {
		v := mustDoc(t)
		if validator != nil {
			v = validator(t)
		}
		collModOn(t, b, branch, &backends.CollModParams{Name: "items", Validator: v, SetValidator: true})
	}
}

// minU is a validator that requires u to be at least |n|.
func minU(n int32) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return mustDoc(t, "u", mustDoc(t, "$gte", n)) }
}

func requireField(field string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return requireExists(t, field) }
}

func setView(value string) edit {
	return func(t *testing.T, b *Backend, branch string) {
		collModOn(t, b, branch, &backends.CollModParams{Name: "v", SetView: true, ViewPipeline: matchPipeline(t, value), SetViewPipeline: true})
	}
}

func addView(value string) edit {
	return func(t *testing.T, b *Backend, branch string) {
		createCollectionOn(t, b, branch, &backends.CreateCollectionParams{Name: "v", ViewOn: "items", ViewPipeline: matchPipeline(t, value)})
	}
}

func dropView(t *testing.T, b *Backend, branch string) {
	db, err := b.Database("testdb@" + branch)
	require.NoError(t, err)
	require.NoError(t, db.DropCollection(context.Background(), &backends.DropCollectionParams{Name: "v"}))
}

func buildMetadataCrissCross(t *testing.T, b *Backend, mode string, withView bool, x1, y1, x2, y2 edit) {
	t.Helper()
	createItems(t, b, mode)
	insertDoc(t, b, "testdb", "items", seedItem(t, 1))
	setValidator(minU(0))(t, b, "main")
	if withView {
		createCollectionOn(t, b, "main", &backends.CreateCollectionParams{Name: "v", ViewOn: "items", ViewPipeline: matchPipeline(t, "c0")})
	}
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "x")
	branchFrom(t, b, "testdb", "main", "y")
	x1(t, b, "x")
	commitBranch(t, b, "testdb", "x", "x1")
	branchFrom(t, b, "testdb", "x", "x1")
	y1(t, b, "y")
	commitBranch(t, b, "testdb", "y", "y1")
	branchFrom(t, b, "testdb", "y", "y1")
	mergeResolving(t, b, "x", "y1", x2)
	mergeResolving(t, b, "y", "x1", y2)
	requireMergeBases(t, b, "x", "y", "y1", "x1")
}

// mergeTips merges |from| into |into| and returns the conflicts, nil when the merge is clean.
func mergeTips(t *testing.T, b *Backend, into, from string) *backends.ConflictsResult {
	t.Helper()
	ctx := context.Background()
	_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "testdb", Into: into, From: from, Message: "merge " + from, Author: "testuser <test@example.com>",
	})
	if err == nil {
		return nil
	}
	res, cerr := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "testdb", Branch: into})
	require.NoError(t, cerr, "merging %s into %s: %v", from, into, err)
	return res
}

var directions = []struct{ into, from string }{{"x", "y"}, {"y", "x"}}

// A validator flagged in the virtual merge base counts as changed on both sides: tips that differ on it conflict,
// although one of them holds the virtual merge base's value. c0's validator requires u >= 0, and x1 and y1 change the
// bound to 1 and 2. The merge merges validators field by field, so x1 and y1 must change the same field to conflict.
func TestVirtualMergeBase_OuterMergeHonorsCatalogFlags(t *testing.T) {
	tests := []struct {
		name     string
		x2, y2   edit
		conflict bool
		// want is the validator after a clean merge.
		want func(t *testing.T) *types.Document
	}{
		{"one tip resolves back to the old validator", setValidator(minU(0)), setValidator(minU(2)), true, nil},
		{"tips resolve alike", setValidator(minU(5)), setValidator(minU(5)), false, minU(5)},
		{"both tips resolve back to the old validator", setValidator(minU(0)), setValidator(minU(0)), false, minU(0)},
	}
	for _, tt := range tests {
		for _, mode := range mergeModes {
			for _, d := range directions {
				t.Run(tt.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					b := newTestBackend(t)
					buildMetadataCrissCross(t, b, mode, false, setValidator(minU(1)), setValidator(minU(2)), tt.x2, tt.y2)
					ours := virtualCatalog(t, b, branchHash(t, b, d.into), "items").Validator
					res := mergeTips(t, b, d.into, d.from)
					if tt.conflict {
						require.NotNil(t, res, "merging %s into %s must conflict", d.from, d.into)
						var collections []string
						for _, mc := range res.Metadata {
							collections = append(collections, mc.Collection)
						}
						require.Contains(t, collections, "items")
						finishWithOurs(t, b, d.into)
						meta := virtualCatalog(t, b, branchHash(t, b, d.into), "items")
						require.True(t, documentsEqual(ours, meta.Validator), "want ours %v, got %v", ours, meta.Validator)
						return
					}
					require.Nil(t, res, "merging %s into %s must be clean", d.from, d.into)
					meta := virtualCatalog(t, b, branchHash(t, b, d.into), "items")
					require.NotNil(t, meta)
					require.True(t, documentsEqual(tt.want(t), meta.Validator), "want %v, got %v", tt.want(t), meta.Validator)
				})
			}
		}
	}
}

func requireSameView(t *testing.T, want, got *viewMeta) {
	t.Helper()
	if want == nil {
		require.Nil(t, got, "the view must stay dropped")
		return
	}
	require.NotNil(t, got, "the view must exist")
	require.True(t, reflectEqual(want.Pipeline, got.Pipeline), "want pipeline %v, got %v", want.Pipeline, got.Pipeline)
}

// A view flagged in the virtual merge base counts as changed on both sides.
func TestVirtualMergeBase_OuterMergeHonorsViewFlags(t *testing.T) {
	tests := []struct {
		name     string
		withView bool
		x1, y1   edit
		x2, y2   edit
		conflict bool
		// want is view v's pipeline after a clean merge.
		want string
	}{
		{"one tip resolves back to the old definition", true, setView("x"), setView("y"), setView("c0"), setView("y"), true, ""},
		{"tips resolve alike", true, setView("x"), setView("y"), setView("same"), setView("same"), false, "same"},
		{"merge bases add the view differently, one tip drops it", false, addView("x"), addView("y"), dropView, setView("y"), true, ""},
	}
	for _, tt := range tests {
		for _, mode := range mergeModes {
			for _, d := range directions {
				t.Run(tt.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					b := newTestBackend(t)
					buildMetadataCrissCross(t, b, mode, tt.withView, tt.x1, tt.y1, tt.x2, tt.y2)
					ours := virtualView(t, b, branchHash(t, b, d.into), "v")
					res := mergeTips(t, b, d.into, d.from)
					if tt.conflict {
						require.NotNil(t, res, "merging %s into %s must conflict", d.from, d.into)
						var views []string
						for _, vc := range res.Views {
							views = append(views, vc.Name)
						}
						require.Contains(t, views, "v")
						finishWithOurs(t, b, d.into)
						requireSameView(t, ours, virtualView(t, b, branchHash(t, b, d.into), "v"))
						return
					}
					require.Nil(t, res, "merging %s into %s must be clean", d.from, d.into)
					vm := virtualView(t, b, branchHash(t, b, d.into), "v")
					require.NotNil(t, vm)
					require.True(t, reflectEqual(matchPipeline(t, tt.want), vm.Pipeline), "want pipeline %q, got %v", tt.want, vm.Pipeline)
				})
			}
		}
	}
}
