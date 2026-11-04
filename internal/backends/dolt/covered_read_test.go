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
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestCoveredReadsFromIndexEntries checks a read of only an index's keys
// builds documents from its entries, keeping each number's type, and fetches
// a document only for a null key.
func TestCoveredReadsFromIndexEntries(t *testing.T) {
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	var docs []*types.Document
	for i := int32(1); i <= 300; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "k", i, "pad", "x")))
	}
	docs = append(docs,
		must.NotFail(types.NewDocument("_id", int32(1001), "k", int64(1001))),
		must.NotFail(types.NewDocument("_id", int32(1002), "k", 1002.5)),
		must.NotFail(types.NewDocument("_id", int32(1003), "k", types.Null)),
	)
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	read := func(params *backends.QueryParams) ([]any, int64) {
		t.Helper()
		ctx, examined := backends.WithExaminedCounts(context.Background())
		res, err := c.Query(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Iter.Close()
		var ks []any
		for {
			_, doc, err := res.Iter.Next()
			if err != nil {
				break
			}
			k, _ := doc.Get("k")
			ks = append(ks, k)
		}
		return ks, examined.Docs()
	}
	gte := func(v any) *types.Document {
		return must.NotFail(types.NewDocument("k", must.NotFail(types.NewDocument("$gte", v))))
	}

	ks, fetched := read(&backends.QueryParams{Filter: gte(int32(1000)), Fields: []string{"k"}})
	if want := []any{int64(1001), 1002.5}; !reflect.DeepEqual(ks, want) || fetched != 0 {
		t.Errorf("range: k %v, %d fetched; want %v, none fetched", ks, fetched, want)
	}

	ks, fetched = read(&backends.QueryParams{Filter: gte(int32(298)), Sort: sortOn("k", 1), LimitHint: 2, Fields: []string{"k"}})
	if want := []any{int32(298), int32(299)}; !reflect.DeepEqual(ks[:2], want) || fetched != 0 {
		t.Errorf("sorted range: k %v, %d fetched; want %v first, none fetched", ks, fetched, want)
	}

	ks, fetched = read(&backends.QueryParams{Filter: must.NotFail(types.NewDocument("k", types.Null)), Fields: []string{"k"}})
	if len(ks) != 2 || fetched != 2 {
		t.Errorf("null: k %v, %d fetched; want the null and the seed document, both fetched", ks, fetched)
	}

	// A covered read looks nothing up, so even a range over the whole
	// collection streams the index.
	if ks, fetched = read(&backends.QueryParams{Filter: gte(int32(0)), Fields: []string{"k"}}); len(ks) != 302 || fetched != 0 {
		t.Errorf("whole range: %d values, %d fetched; want 302, none fetched", len(ks), fetched)
	}

	if _, fetched = read(&backends.QueryParams{Filter: gte(int32(1000)), Fields: []string{"k", "pad"}}); fetched != 2 {
		t.Errorf("a field outside the index: %d fetched, want 2", fetched)
	}
}
