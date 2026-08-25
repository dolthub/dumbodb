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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// TestSeekCounter_PointLookupReadsFewerNodesThanScan validates that the
// instrumented NodeStore is live on the real read path and that an indexed
// point lookup fetches strictly fewer prolly-tree nodes than a full scan of the
// same data. This is the single-scale proof that a served query does sub-linear
// storage work; the multi-scale log-N growth assertion lives with the scaling
// test (workspace-da6.1 step 4).
func TestSeekCounter_PointLookupReadsFewerNodesThanScan(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	db, err := b.Database("perfdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("items")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	const n = 5000
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, mustDoc(t, "_id", int32(i), "k", int32(i)))
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}
	createIndex(t, ctx, coll, "k_idx", "k")

	seekCtx, seek := WithSeekCounter(ctx)
	found := drainQuery(t, seekCtx, coll, &backends.QueryParams{Filter: mustDoc(t, "k", int32(n/2))})
	if len(found) != 1 {
		t.Fatalf("point lookup returned %d docs, want 1", len(found))
	}
	nodesSeek := seek.Nodes()

	scanCtx, scan := WithSeekCounter(ctx)
	all := drainQuery(t, scanCtx, coll, &backends.QueryParams{Filter: mustDoc(t)})
	if len(all) != n {
		t.Fatalf("full scan returned %d docs, want %d", len(all), n)
	}
	nodesScan := scan.Nodes()

	t.Logf("N=%d  point-lookup nodes=%d  full-scan nodes=%d", n, nodesSeek, nodesScan)

	if nodesSeek == 0 {
		t.Fatal("instrumentation dead: point lookup counted 0 node reads through the counter context")
	}
	if nodesScan == 0 {
		t.Fatal("instrumentation dead: full scan counted 0 node reads through the counter context")
	}
	if nodesSeek >= nodesScan {
		t.Fatalf("point lookup did not read fewer nodes than a full scan: seek=%d scan=%d", nodesSeek, nodesScan)
	}
}

// TestSeekCounter_InertWithoutContext confirms the production path is unmetered
// unless a caller opts in: a query run on a bare context leaves no counter to
// accrue to, so the instrumented store simply delegates.
func TestSeekCounter_InertWithoutContext(t *testing.T) {
	ctx := context.Background()
	if c := seekCounterFrom(ctx); c != nil {
		t.Fatalf("bare context carried a seekCounter: %v", c)
	}
}
