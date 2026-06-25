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

// Behavior B3 of docs/design/secondary-index-structural-sharing.md,
// plus the chunk-walk helpers shared by the structural-sharing tests.

import (
	"context"
	"fmt"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Returns the root address plus every address referenced beneath it.
func chunkAddresses(t *testing.T, ctx context.Context, m prolly.Map) map[hash.Hash]struct{} {
	t.Helper()
	out := map[hash.Hash]struct{}{m.HashOf(): {}}
	if err := m.WalkAddresses(ctx, func(_ context.Context, addr hash.Hash) error {
		out[addr] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("WalkAddresses: %v", err)
	}
	return out
}

func countSharedChunks(a, b map[hash.Hash]struct{}) int {
	n := 0
	for h := range a {
		if _, ok := b[h]; ok {
			n++
		}
	}
	return n
}

// Opens the index map persisted in the branch's DTBL, bypassing in-memory
// state.
func indexMapOnBranch(t *testing.T, ctx context.Context, b *Backend, dbName, branch, collName, idxName string) prolly.Map {
	t.Helper()

	am := indexAMOnDisk(t, ctx, b, dbName, branch, collName)
	entryHash, err := am.Get(ctx, idxName)
	if err != nil {
		t.Fatalf("index AM Get(%q): %v", idxName, err)
	}
	if entryHash.IsEmpty() {
		t.Fatalf("index %q not present on branch %q", idxName, branch)
	}

	state, err := b.getOrOpenDB(ctx, dbName, false)
	if err != nil || state == nil {
		t.Fatalf("getOrOpenDB(%q): %v", dbName, err)
	}
	resolved, err := resolveIndexEntry(ctx, state.ns, entryHash)
	if err != nil {
		t.Fatalf("resolveIndexEntry: %v", err)
	}
	m, err := openIndexMap(ctx, state.vs, state.ns, resolved.mapRoot)
	if err != nil {
		t.Fatalf("openIndexMap: %v", err)
	}
	return m
}

func bulkInsert(t *testing.T, ctx context.Context, coll backends.Collection, idBase int32, n int, prefix string) {
	t.Helper()
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, must.NotFail(types.NewDocument(
			"_id", idBase+int32(i),
			"field", fmt.Sprintf("%s%05d", prefix, i),
		)))
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("bulk InsertAll(%s): %v", prefix, err)
	}
}

// The merged index tree must physically reuse leaf chunks from BOTH parents
// (B3).
func TestMergedIndexChunkReuseFromBothParents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	const docsPerSide = 3000

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base: seed + index")

	branchFrom(t, b, "testdb", "main", "feat")

	bulkInsert(t, ctx, collAt(t, b, "testdb", "main", "items"), 1000, docsPerSide, "a")
	commitBranch(t, b, "testdb", "main", "main: a-half")

	bulkInsert(t, ctx, collAt(t, b, "testdb", "feat", "items"), 100000, docsPerSide, "n")
	commitBranch(t, b, "testdb", "feat", "feat: n-half")

	mainChunks := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field"))
	featChunks := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "feat", "items", "by_field"))

	// Each side must span multiple chunks or sharing is unmeasurable.
	if len(mainChunks) < 3 || len(featChunks) < 3 {
		t.Fatalf("index trees too small to measure sharing (main=%d chunks, feat=%d chunks); raise docsPerSide",
			len(mainChunks), len(featChunks))
	}

	mergeBranches(t, b, "testdb", "main", "feat")

	mergedChunks := chunkAddresses(t, ctx, indexMapOnBranch(t, ctx, b, "testdb", "main", "items", "by_field"))

	sharedWithMain := countSharedChunks(mergedChunks, mainChunks)
	sharedWithFeat := countSharedChunks(mergedChunks, featChunks)

	t.Logf("chunks: main=%d feat=%d merged=%d sharedWithMain=%d sharedWithFeat=%d",
		len(mainChunks), len(featChunks), len(mergedChunks), sharedWithMain, sharedWithFeat)

	if sharedWithMain == 0 {
		t.Errorf("merged index shares no chunks with the into parent; expected leaf reuse")
	}
	if sharedWithFeat == 0 {
		t.Errorf("merged index shares no chunks with the from parent; expected leaf reuse")
	}
}
