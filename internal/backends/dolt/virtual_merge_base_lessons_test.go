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
	"time"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// Section K: lessons from Dolt bugs found after virtual merge bases shipped. The histories are outerScenario's:
//
//	c0 -- x1 -- x2    x
//	  \-- y1 -- y2    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)

// b4 is B4: x1 and y1 set a to x and y; x2 resolves it back to c0's value and y2 to y.
func b4() outerScenario {
	return outerScenario{x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("seed")), y2: setTo(itemA("y"))}
}

// b2 is B2: x1 and y1 set a to x and y; x2 and y2 resolve it to "x wins" and "y wins".
func b2() outerScenario {
	return outerScenario{x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("x wins")), y2: setTo(itemA("y wins"))}
}

// inOtherTimeZoneLater runs the rest of the test in another local time zone, with commits dated an hour later.
func inOtherTimeZoneLater(t *testing.T) {
	t.Helper()
	local, now := time.Local, datas.CommitNow
	time.Local = time.FixedZone("UTC+9", 9*60*60)
	datas.CommitNow = func() time.Time { return now().Add(time.Hour) }
	t.Cleanup(func() { time.Local, datas.CommitNow = local, now })
}

func mergeBaseOf(t *testing.T, b *Backend, into, from string) (hash.Hash, virtualBaseFlags) {
	t.Helper()
	db, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	base, flags, ok, err := db.mergeBaseWithFlags(context.Background(), branchHash(t, b, into), branchHash(t, b, from))
	require.NoError(t, err)
	require.True(t, ok)
	return base, flags
}

// K1a (Dolt #12198): the virtual merge base does not depend on when, where or by which process it is built.
func TestVirtualMergeBase_SameWhoeverBuildsIt(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			b := openRestartableBackend(t, dir)
			buildOuterScenario(t, b, mode, b4())
			before, beforeFlags := mergeBaseOf(t, b, "x", "y")
			b.Close()

			inOtherTimeZoneLater(t)
			b = openRestartableBackend(t, dir)
			t.Cleanup(b.Close)
			after, afterFlags := mergeBaseOf(t, b, "y", "x")
			require.Equal(t, before, after)
			require.Equal(t, beforeFlags, afterFlags)
		})
	}
}

// K1b: B4 paused, restarted in another time zone, then continued.
func TestVirtualMergeBase_ContinueInOtherTimeZone(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			b := openRestartableBackend(t, dir)
			buildOuterScenario(t, b, mode, b4())
			_, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
				DBName: "testdb", Into: "x", From: "y", Message: "merge y", Author: "testuser <test@example.com>",
			})
			require.Error(t, err, "merging y into x must conflict")
			before := conflictOnDoc(t, b, "x", 1)
			b.Close()

			inOtherTimeZoneLater(t)
			b = openRestartableBackend(t, dir)
			t.Cleanup(b.Close)
			after := conflictOnDoc(t, b, "x", 1)
			requireSameDoc(t, before.Base, after.Base, "base of the restored conflict")
			requireSameDoc(t, before.Ours, after.Ours, "ours of the restored conflict")
			requireSameDoc(t, before.Theirs, after.Theirs, "theirs of the restored conflict")
			resolveOursAndContinue(t, b, "x")
			requireSameDoc(t, itemA("seed")(t), virtualBaseItems(t, b, branchHash(t, b, "x"))[1], "document 1 after continuing")
		})
	}
}

// K2a and K2b (Dolt #12330, #12333, #12354): the conflict's sides follow the merge's direction, and decode to each
// side's own values even when x2 also changes items' indexes or validator, which y2 does not.
func TestVirtualMergeBase_ConflictSidesFollowTheDirection(t *testing.T) {
	variants := []struct {
		name string
		x2   edit
	}{
		{"same metadata", nil},
		{"x2 adds an index", func(t *testing.T, b *Backend, branch string) {
			createIndex(t, context.Background(), collAt(t, b, "testdb", branch, "items"), "by_b", "b")
		}},
		{"x2 sets a validator", setValidator(requireField("a"))},
	}
	for _, v := range variants {
		for _, mode := range mergeModes {
			for _, d := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
				t.Run(v.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					b := newTestBackend(t)
					s := b2()
					if v.x2 != nil {
						s.x2 = editAll(s.x2, v.x2)
					}
					buildOuterScenario(t, b, mode, s)
					_, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
						DBName: "testdb", Into: d.into, From: d.from, Message: "merge", Author: "testuser <test@example.com>",
					})
					require.Error(t, err, "the merge must conflict")
					tips := map[string]string{"x": "x wins", "y": "y wins"}
					c := conflictOnDoc(t, b, d.into, 1)
					requireSameDoc(t, itemA(tips[d.into])(t), c.Ours, "ours")
					requireSameDoc(t, itemA(tips[d.from])(t), c.Theirs, "theirs")
					requireSameDoc(t, itemA("seed")(t), c.Base, "base")
				})
			}
		}
	}
}

// K2c: x2 and y2 also set different merge modes, both other than c0's, a governing metadata conflict that defers items. Once it is
// resolved, items merges against the same virtual merge base as the rest.
func TestVirtualMergeBase_DeferredCollectionUsesTheVirtualMergeBase(t *testing.T) {
	setMode := func(mode MergeMode) edit {
		return func(t *testing.T, b *Backend, branch string) {
			collModOn(t, b, branch, &backends.CollModParams{Name: "items", MergeMode: string(mode)})
		}
	}
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			b := newTestBackend(t)
			s := b2()
			others := otherMergeModes(mode)
			s.x2 = editAll(s.x2, setMode(others[0]))
			s.y2 = editAll(s.y2, setMode(others[1]))
			buildOuterScenario(t, b, mode, s)
			ctx := context.Background()
			_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "x", From: "y", Message: "merge y", Author: "testuser <test@example.com>",
			})
			require.Error(t, err, "the merge must conflict on items' merge mode")
			res, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "testdb", Branch: "x"})
			require.NoError(t, err)
			require.Len(t, res.Metadata, 1)
			require.Empty(t, res.Collections, "items' documents wait for its metadata")

			resolveAllWithOurs(t, b, "x")
			_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "x", Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
			})
			require.Error(t, err, "items' documents must conflict once its metadata is resolved")
			c := conflictOnDoc(t, b, "x", 1)
			requireSameDoc(t, itemA("seed")(t), c.Base, "base")
			requireSameDoc(t, itemA("x wins")(t), c.Ours, "ours")
			requireSameDoc(t, itemA("y wins")(t), c.Theirs, "theirs")
		})
	}
}

// structuralHistory is the A1 history with schema branches that make a structural change, which main and feature
// make too before merging them:
//
//	c1 -- c2 -- m1 -- merge s<i>...    main
//	 |     \-- f1 -- merge s<i>...     feature
//	 \-- s<i>                          schema<i>, one per |schemas|
//
// c1 creates items in |mode|. Each schema<i> applies schemas[i]. c2 adds |c2Docs| and applies |c2| when set. m1 and
// f1 apply |tips|, and add documents 30 and 40.
type structuralHistory struct {
	name    string
	schemas []edit
	c2Docs  []func(t *testing.T) *types.Document
	c2      edit
	tips    edit
}

func buildStructuralHistory(t *testing.T, b *Backend, mode string, h structuralHistory) {
	t.Helper()
	createItems(t, b, mode)
	commitDB(t, b, "testdb", "c1")
	var bases []string
	for i, schema := range h.schemas {
		branch := fmt.Sprintf("schema%d", i+1)
		branchFrom(t, b, "testdb", "main", branch)
		schema(t, b, branch)
		commitBranch(t, b, "testdb", branch, "s")
		bases = append([]string{branch}, bases...)
	}
	for _, doc := range h.c2Docs {
		insertDoc(t, b, "testdb", "items", doc(t))
	}
	if h.c2 != nil {
		h.c2(t, b, "main")
	}
	commitDB(t, b, "testdb", "c2")
	branchFrom(t, b, "testdb", "main", "fork")
	branchFrom(t, b, "testdb", "main", "feature")
	for branch, id := range map[string]int32{"main": 30, "feature": 40} {
		h.tips(t, b, branch)
		insertDoc(t, b, "testdb@"+branch, "items", uDoc(t, id, id))
		commitBranch(t, b, "testdb", branch, branch+" tip")
	}
	for _, branch := range []string{"main", "feature"} {
		for i := range h.schemas {
			mergeResolving(t, b, branch, fmt.Sprintf("schema%d", i+1), nil)
		}
	}
	bases = append([]string{"fork"}, bases...)
	if len(h.schemas) > 0 {
		requireMergeBases(t, b, "main", "feature", bases...)
	}
}

func otherMergeModes(mode string) []MergeMode {
	var others []MergeMode
	for _, other := range mergeModes {
		if other != mode {
			others = append(others, MergeMode(other))
		}
	}
	return others
}

func setMergeMode(mode MergeMode) edit {
	return func(t *testing.T, b *Backend, branch string) {
		collModOn(t, b, branch, &backends.CollModParams{Name: "items", MergeMode: string(mode)})
	}
}

// K3 (Dolt #12357, #12360): a change both tips made the same way is one-sided between the merge bases, one of which
// predates it.
// The schema branches set a merge mode other than c0's, and in K3e c2 sets a third one.
func TestVirtualMergeBase_ChangeTheTipsShare(t *testing.T) {
	seeds := []func(t *testing.T) *types.Document{uItem(1, 1), uItem(2, 2)}
	histories := func(mode string) []structuralHistory {
		others := otherMergeModes(mode)
		schemaMode := setMergeMode(others[0])
		return []structuralHistory{
			{name: "K3a merge mode", schemas: []edit{schemaMode}, c2Docs: seeds, tips: schemaMode},
			{name: "K3b validator", schemas: []edit{setValidator(minU(0))}, c2Docs: seeds, tips: setValidator(minU(0))},
			{name: "K3d two schema branches", schemas: []edit{schemaMode, schemaMode}, c2Docs: seeds, tips: schemaMode},
			{
				name: "K3e merge bases set different merge modes", schemas: []edit{schemaMode}, c2Docs: seeds,
				c2: setMergeMode(others[1]), tips: schemaMode,
			},
		}
	}
	for _, mode := range mergeModes {
		for _, h := range histories(mode) {
			for _, d := range []struct{ into, from string }{{"main", "feature"}, {"feature", "main"}} {
				t.Run(h.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					useTestCommitClock(t)
					b := newTestBackend(t)
					buildStructuralHistory(t, b, mode, h)
					requireValidMergeBase(t, b, d.into, d.from)
					mergeBranches(t, b, "testdb", d.into, d.from)
					requireItems(t, b, d.into, map[int32]*types.Document{
						1: uDoc(t, 1, 1), 2: uDoc(t, 2, 2), 30: uDoc(t, 30, 30), 40: uDoc(t, 40, 40),
					})
					want := virtualCatalog(t, b, branchHash(t, b, "schema1"), "items")
					got := virtualCatalog(t, b, branchHash(t, b, d.into), "items")
					require.Equal(t, want.MergeMode, got.MergeMode)
					require.True(t, documentsEqual(want.Validator, got.Validator), "validator %v", got.Validator)
				})
			}
		}
	}
}

// K3c: the schema branch adds a unique index on u, which c2's documents 1 and 2 violate; main and feature give
// document 2 u = 6 and add the index too. The fold evicts one of the colliding documents and flags it, so the
// touched modes conflict on it, and the divergent modes merge.
func TestVirtualMergeBase_UniqueIndexTheTipsShare(t *testing.T) {
	h := structuralHistory{
		schemas: []edit{createUniqueIndex},
		c2Docs:  []func(t *testing.T) *types.Document{uItem(1, 5), uItem(2, 5)},
		tips:    editAll(setTo(uItem(2, 6)), createUniqueIndex),
	}
	for _, mode := range mergeModes {
		for _, d := range []struct{ into, from string }{{"main", "feature"}, {"feature", "main"}} {
			t.Run(mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
				useTestCommitClock(t)
				b := newTestBackend(t)
				buildStructuralHistory(t, b, mode, h)
				requireValidMergeBase(t, b, d.into, d.from)
				_, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
					DBName: "testdb", Into: d.into, From: d.from, Message: "merge", Author: "testuser <test@example.com>",
				})
				if mode == string(MergeModeDocumentTouched) || mode == string(MergeModeFieldTouched) {
					require.Error(t, err, "the touched modes must conflict on the evicted document")
					return
				}
				require.NoError(t, err)
				requireItems(t, b, d.into, map[int32]*types.Document{
					1: uDoc(t, 1, 5), 2: uDoc(t, 2, 6), 30: uDoc(t, 30, 30), 40: uDoc(t, 40, 40),
				})
			})
		}
	}
}

// K4 (Dolt #12421, #12423): x1 and y1 both create items and add document 100 to it; x1 also gives items a validator,
// which y2 then adds too. c0 holds only collection other.
func bothCreateItems(mode string) outerScenario {
	create := func(validator bool) edit {
		return func(t *testing.T, b *Backend, branch string) {
			params := &backends.CreateCollectionParams{Name: "items", MergeMode: mode}
			if validator {
				params.Validator = requireExists(t, "v")
			}
			createCollectionOn(t, b, branch, params)
			insertDoc(t, b, "testdb@"+branch, "items", doc100(t))
		}
	}
	return outerScenario{
		setup: func(t *testing.T, b *Backend, mode string) {
			insertDoc(t, b, "testdb", "other", mustDoc(t, "_id", int32(1)))
		},
		x1: create(true), y1: create(false), x2: editAll(), y2: setValidator(requireField("v")),
	}
}

func doc100(t *testing.T) *types.Document { return mustDoc(t, "_id", int32(100), "v", int32(1)) }

func TestVirtualMergeBase_MergeBasesCreateTheCollection(t *testing.T) {
	touchedConflict := func(doc func(t *testing.T) *types.Document) map[string]outerResult {
		divergent := merges(nil)
		if doc != nil {
			divergent = merges(doc(t))
		}
		return outcomes(conflicts(), conflicts(), divergent, divergent)
	}
	scenarios := []struct {
		name   string
		id     int32
		change func(s *outerScenario)
		want   func(t *testing.T) map[string]outerResult
	}{
		// K4a: document 100, added alike on both merge bases, is flagged in the touched modes (D5).
		{"tips agree", 100, nil, func(t *testing.T) map[string]outerResult { return touchedConflict(doc100) }},
		// K4b: the virtual merge base keeps the documents the bases agree on, so x's delete merges.
		{"x deletes document 100", 100, func(s *outerScenario) { s.x2 = deleteItem(100) },
			func(t *testing.T) map[string]outerResult { return touchedConflict(nil) }},
		// K4c
		{"tips add document 3 differently", 3, func(s *outerScenario) {
			s.x2 = addItem(doc3("x"))
			s.y2 = editAll(s.y2, addItem(doc3("y")))
		}, func(t *testing.T) map[string]outerResult {
			return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
		}},
	}
	for _, sc := range scenarios {
		for _, mode := range mergeModes {
			for _, d := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
				t.Run(sc.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					b := newTestBackend(t)
					s := bothCreateItems(mode)
					if sc.change != nil {
						sc.change(&s)
					}
					buildOuterScenario(t, b, mode, s)
					requireOuterResult(t, b, d.into, d.from, sc.id, sc.want(t)[mode])
					if sc.id == 3 {
						require.Nil(t, conflictOnDoc(t, b, d.into, 3).Base, "document 3 has no base")
					}
				})
			}
		}
	}
}

// K4d: y3 changes items' validator again, so the tips differ on the validator the virtual merge base flags.
func TestVirtualMergeBase_MergeBasesCreateTheCollectionThenItChanges(t *testing.T) {
	for _, mode := range mergeModes {
		for _, d := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
			t.Run(mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
				b := newTestBackend(t)
				s := bothCreateItems(mode)
				s.y3 = setValidator(requireField("w"))
				buildOuterScenario(t, b, mode, s)
				res := mergeTips(t, b, d.into, d.from)
				require.NotNil(t, res, "the merge must conflict")
				require.Len(t, res.Metadata, 1, "the validators must conflict")
			})
		}
	}
}

// K4e: x1 and y1 create items in different merge modes, and the tips agree on x1's. The merge keeps it, as H9.
func TestVirtualMergeBase_MergeBasesCreateTheCollectionInDifferentModes(t *testing.T) {
	for _, mode := range mergeModes {
		for _, d := range []struct{ into, from string }{{"x", "y"}, {"y", "x"}} {
			t.Run(mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
				b := newTestBackend(t)
				s := bothCreateItems(mode)
				yMode := otherMergeModes(mode)[0]
				s.y1 = func(t *testing.T, b *Backend, branch string) {
					createCollectionOn(t, b, branch, &backends.CreateCollectionParams{Name: "items", MergeMode: string(yMode)})
					insertDoc(t, b, "testdb@"+branch, "items", doc100(t))
				}
				s.y2 = editAll(s.y2, setMergeMode(MergeMode(mode)))
				buildOuterScenario(t, b, mode, s)
				mergeResolving(t, b, d.into, d.from, nil)
				meta := virtualCatalog(t, b, branchHash(t, b, d.into), "items")
				require.Equal(t, mode, meta.MergeMode)
				requireSameDoc(t, doc100(t), virtualBaseItems(t, b, branchHash(t, b, d.into))[100], "document 100")
			})
		}
	}
}
