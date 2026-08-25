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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// seedIndexedCollection seeds n docs {_id:i, k:i} with a secondary index on k.
func seedIndexedCollection(t *testing.T, b *Backend, dbName string, n int) backends.Collection {
	t.Helper()
	db, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("items")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	const batch = 10000
	buf := make([]*types.Document, 0, batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if _, err := coll.InsertAll(context.Background(), &backends.InsertAllParams{Docs: buf}); err != nil {
			t.Fatalf("InsertAll: %v", err)
		}
		buf = buf[:0]
	}
	for i := 0; i < n; i++ {
		buf = append(buf, mustDoc(t, "_id", int32(i), "k", int32(i)))
		if len(buf) == batch {
			flush()
		}
	}
	flush()
	createIndex(t, context.Background(), coll, "k_idx", "k")
	return coll
}

func pointLookupNodes(t *testing.T, coll backends.Collection, k int) int64 {
	t.Helper()
	ctx, ctr := WithSeekCounter(context.Background())
	got := drainQuery(t, ctx, coll, &backends.QueryParams{Filter: mustDoc(t, "k", int32(k))})
	if len(got) != 1 {
		t.Fatalf("point lookup k=%d returned %d docs, want 1", k, len(got))
	}
	return ctr.Nodes()
}

func TestSeekScaling_PointLookupIsLogarithmic(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds up to 100k docs; skipped under -short")
	}
	b := newTestBackend(t)

	scales := []int{1000, 10000, 100000}
	nodes := make([]int64, len(scales))
	for i, n := range scales {
		coll := seedIndexedCollection(t, b, fmt.Sprintf("scale_%d", n), n)
		nodes[i] = pointLookupNodes(t, coll, n/2)
		t.Logf("N=%-7d point-lookup nodes=%d", n, nodes[i])
	}

	bigColl := seedIndexedCollection(t, b, "scale_scan", scales[len(scales)-1])
	scanCtx, scanCtr := WithSeekCounter(context.Background())
	all := drainQuery(t, scanCtx, bigColl, &backends.QueryParams{Filter: mustDoc(t)})
	if len(all) != scales[len(scales)-1] {
		t.Fatalf("scan returned %d, want %d", len(all), scales[len(scales)-1])
	}
	scanNodes := scanCtr.Nodes()
	t.Logf("N=%-7d full-scan nodes=%d", scales[len(scales)-1], scanNodes)

	growth := nodes[len(nodes)-1] - nodes[0]
	const maxLevelGrowth = 4
	if growth > maxLevelGrowth {
		t.Fatalf("point-lookup node fetches grew by %d across 100x data (want <= %d): %v",
			growth, maxLevelGrowth, nodes)
	}
	if scanNodes < 4*nodes[len(nodes)-1] {
		t.Fatalf("scan (%d) not clearly costlier than seek (%d) at N=%d; log-vs-linear gap unproven",
			scanNodes, nodes[len(nodes)-1], scales[len(scales)-1])
	}
}

// seedCollatedCollection seeds n docs {_id:i, k:"k<i>"} with a secondary index
// on k under the given collation (so it stores ICU sort keys).
func seedCollatedCollection(t *testing.T, b *Backend, dbName string, n int, coll_ *types.Document) backends.Collection {
	t.Helper()
	db, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("items")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	const batch = 10000
	buf := make([]*types.Document, 0, batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if _, err := coll.InsertAll(context.Background(), &backends.InsertAllParams{Docs: buf}); err != nil {
			t.Fatalf("InsertAll: %v", err)
		}
		buf = buf[:0]
	}
	for i := 0; i < n; i++ {
		buf = append(buf, mustDoc(t, "_id", int32(i), "k", fmt.Sprintf("k%07d", i)))
		if len(buf) == batch {
			flush()
		}
	}
	flush()
	if _, err := coll.CreateIndexes(context.Background(), &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:      "k_idx",
			Key:       []backends.IndexKeyPair{{Field: "k"}},
			Collation: coll_,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes (collated): %v", err)
	}
	return coll
}

func TestSeekScaling_CollatedPointLookupIsLogarithmic(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds up to 100k docs; skipped under -short")
	}
	b := newTestBackend(t)
	colDoc := mustDoc(t, "locale", "fr_CA")

	scales := []int{1000, 100000}
	nodes := make([]int64, len(scales))
	for i, n := range scales {
		coll := seedCollatedCollection(t, b, fmt.Sprintf("cscale_%d", n), n, colDoc)
		ctx, ctr := WithSeekCounter(context.Background())
		got := drainQuery(t, ctx, coll, &backends.QueryParams{
			Filter:    mustDoc(t, "k", fmt.Sprintf("k%07d", n/2)),
			Collation: colDoc,
			Collated:  true,
		})
		if len(got) != 1 {
			t.Fatalf("collated point lookup at N=%d returned %d docs, want 1", n, len(got))
		}
		nodes[i] = ctr.Nodes()
		t.Logf("N=%-7d collated point-lookup nodes=%d", n, nodes[i])
	}

	if nodes[0] == 0 {
		t.Fatal("collated lookup counted 0 node reads: not served by the sort-key index")
	}
	growth := nodes[len(nodes)-1] - nodes[0]
	const maxLevelGrowth = 4
	if growth > maxLevelGrowth {
		t.Fatalf("collated point-lookup node fetches grew by %d across 100x data (want <= %d): %v",
			growth, maxLevelGrowth, nodes)
	}
}
