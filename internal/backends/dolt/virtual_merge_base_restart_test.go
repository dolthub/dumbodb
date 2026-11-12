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
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// A conflicted merge with a virtual merge base must survive a restart, and a GC that drops the virtual merge base's
// dangling commit. Each test builds B2 of outerScenario:
//
//	c0 -- x1 -- x2    x
//	  \-- y1 -- y2    y
//	x2 = merge(x1, y1), y2 = merge(y1, x1)
//
// x1 and y1 set a to x and y; x2 and y2 resolve it to "x wins" and "y wins". Merging y into x conflicts on
// document 1.

func openRestartableBackend(t *testing.T, dir string) *Backend {
	t.Helper()
	b, err := newBackend(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	return b
}

func conflictOnDoc(t *testing.T, b *Backend, branch string, id int32) backends.ConflictInfo {
	t.Helper()
	res, err := b.DumboDBConflicts(context.Background(), &backends.ConflictsParams{DBName: "testdb", Branch: branch})
	require.NoError(t, err)
	for _, cc := range res.Collections {
		for _, c := range cc.Conflicts {
			for _, doc := range []*types.Document{c.Ours, c.Theirs, c.Base} {
				if doc == nil {
					continue
				}
				if v, _ := doc.Get("_id"); v == id {
					return c
				}
			}
		}
	}
	t.Fatalf("no conflict on document %d on %s", id, branch)
	return backends.ConflictInfo{}
}

func mergeYIntoXConflicted(t *testing.T, b *Backend, mode string) {
	t.Helper()
	s := outerScenario{x1: setTo(itemA("x")), y1: setTo(itemA("y")), x2: setTo(itemA("x wins")), y2: setTo(itemA("y wins"))}
	buildOuterScenario(t, b, mode, s)
	_, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
		DBName: "testdb", Into: "x", From: "y", Message: "merge y", Author: "testuser <test@example.com>",
	})
	require.Error(t, err, "merging y into x must conflict")
}

func resolveOursAndContinue(t *testing.T, b *Backend, branch string) {
	t.Helper()
	resolveAllWithOurs(t, b, branch)
	_, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
		DBName: "testdb", Into: branch, Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
	})
	require.NoError(t, err)
}

func TestVirtualMergeBase_ConflictsSurviveRestart(t *testing.T) {
	for _, gc := range []bool{false, true} {
		for _, mode := range mergeModes {
			name := mode + "/restart"
			if gc {
				name = mode + "/gc and restart"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				b := openRestartableBackend(t, dir)
				mergeYIntoXConflicted(t, b, mode)
				before := conflictOnDoc(t, b, "x", 1)
				requireSameDoc(t, itemA("seed")(t), before.Base, "base of the conflict")
				if gc {
					ctx := ctxWithSession(t, b, "test-lsid-restart-gc")
					_, err := b.DumboDBGC(ctx, &backends.GCParams{DBName: "testdb", Mode: "full"})
					require.NoError(t, err)
				}
				b.Close()

				b = openRestartableBackend(t, dir)
				t.Cleanup(b.Close)
				after := conflictOnDoc(t, b, "x", 1)
				requireSameDoc(t, before.Base, after.Base, "base of the restored conflict")
				requireSameDoc(t, before.Ours, after.Ours, "ours of the restored conflict")
				requireSameDoc(t, before.Theirs, after.Theirs, "theirs of the restored conflict")

				resolveOursAndContinue(t, b, "x")
				docs := virtualBaseItems(t, b, branchHash(t, b, "x"))
				requireSameDoc(t, itemA("x wins")(t), docs[1], "document 1 after continuing")
			})
		}
	}
}

// J2: x2 and y2 also set different validators on items, a governing metadata conflict that defers merging items'
// documents until it is resolved. Resolving it after the restart must still conflict on document 1, against the
// virtual merge base's document.
func TestVirtualMergeBase_DeferredConflictsSurviveRestart(t *testing.T) {
	for _, gc := range []bool{false, true} {
		for _, mode := range mergeModes {
			name := mode + "/restart"
			if gc {
				name = mode + "/gc and restart"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				b := openRestartableBackend(t, dir)
				s := outerScenario{
					x1: setTo(itemA("x")), y1: setTo(itemA("y")),
					x2: editAll(setTo(itemA("x wins")), setValidator(requireField("a"))),
					y2: editAll(setTo(itemA("y wins")), setValidator(requireField("b"))),
				}
				buildOuterScenario(t, b, mode, s)
				ctx := context.Background()
				_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
					DBName: "testdb", Into: "x", From: "y", Message: "merge y", Author: "testuser <test@example.com>",
				})
				require.Error(t, err, "merging y into x must conflict on items' metadata")
				if gc {
					gcCtx := ctxWithSession(t, b, "test-lsid-deferred-gc")
					_, err := b.DumboDBGC(gcCtx, &backends.GCParams{DBName: "testdb", Mode: "full"})
					require.NoError(t, err)
				}
				b.Close()

				b = openRestartableBackend(t, dir)
				t.Cleanup(b.Close)
				resolveAllWithOurs(t, b, "x")
				_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
					DBName: "testdb", Into: "x", Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
				})
				require.Error(t, err, "items' documents must conflict once the metadata conflict is resolved")
				c := conflictOnDoc(t, b, "x", 1)
				requireSameDoc(t, itemA("seed")(t), c.Base, "base of the deferred conflict")

				resolveOursAndContinue(t, b, "x")
				docs := virtualBaseItems(t, b, branchHash(t, b, "x"))
				requireSameDoc(t, itemA("x wins")(t), docs[1], "document 1 after continuing")
			})
		}
	}
}
