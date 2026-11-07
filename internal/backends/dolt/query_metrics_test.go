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
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestQueryExecutorCounters checks the keys and documents each read counts,
// server-wide and for the operation whose context it runs under.
func TestQueryExecutorCounters(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, int32(30)), kDoc(2, int32(10)), kDoc(3, int32(20)), kDoc(4, int32(10)),
	)
	insertFiller(t, c, 20) // with the seed document, 25 in all
	eq := func(field string, v any) *types.Document { return must.NotFail(types.NewDocument(field, v)) }
	query := func(ctx context.Context, params *backends.QueryParams) {
		res, err := c.Query(ctx, params)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		defer res.Iter.Close()
		for {
			if _, _, err := res.Iter.Next(); err != nil {
				return
			}
		}
	}

	for _, tc := range []struct {
		name       string
		run        func(ctx context.Context)
		keys, docs int64
	}{
		{"collection scan", func(ctx context.Context) { query(ctx, &backends.QueryParams{Filter: eq("x", int32(1))}) }, 0, 25},
		{"index equality", func(ctx context.Context) { query(ctx, &backends.QueryParams{Filter: eq("k", int32(10))}) }, 2, 2},
		// The clustered _id index reads the document itself: no keys.
		{"_id equality", func(ctx context.Context) { query(ctx, &backends.QueryParams{Filter: eq("_id", int32(3))}) }, 0, 1},
		{"_id miss", func(ctx context.Context) { query(ctx, &backends.QueryParams{Filter: eq("_id", int32(99))}) }, 0, 0},
		{"index-only count", func(ctx context.Context) {
			if _, err := c.Count(ctx, &backends.CountParams{Filter: eq("k", int32(10))}); err != nil {
				t.Fatal(err)
			}
		}, 2, 0},
	} {
		keys, docs := queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
		ctx, examined := backends.WithExaminedCounts(context.Background())
		tc.run(ctx)
		gotKeys, gotDocs := queryExecutorCounters.keys.Load()-keys, queryExecutorCounters.docs.Load()-docs
		if gotKeys != tc.keys || gotDocs != tc.docs {
			t.Errorf("%s: examined %d keys, %d docs; want %d, %d", tc.name, gotKeys, gotDocs, tc.keys, tc.docs)
		}
		if examined.Keys() != tc.keys || examined.Docs() != tc.docs {
			t.Errorf("%s: operation examined %d keys, %d docs; want %d, %d", tc.name, examined.Keys(), examined.Docs(), tc.keys, tc.docs)
		}
	}
}
