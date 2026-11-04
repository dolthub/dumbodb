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

// TestDistinctScanSkipsBetweenValues checks a distinct scan reads about one
// index key per value and restores values from their entries, fetching only
// the members of a subdocument group.
func TestDistinctScanSkipsBetweenValues(t *testing.T) {
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "grp", Key: []backends.IndexKeyPair{{Field: "grp"}}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	var docs []*types.Document
	for i := int32(1); i <= 10000; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", i%1000)))
	}
	docs = append(docs,
		must.NotFail(types.NewDocument("_id", int32(20001), "grp", int64(5000))),
		must.NotFail(types.NewDocument("_id", int32(20002), "grp", 5000.5)),
		must.NotFail(types.NewDocument("_id", int32(20003), "grp", must.NotFail(types.NewDocument("a", int32(1))))),
		must.NotFail(types.NewDocument("_id", int32(20004), "grp", must.NotFail(types.NewDocument("a", int32(2))))),
	)
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	scan := func(filter *types.Document) ([]any, *backends.ExaminedCounts) {
		t.Helper()
		ctx, examined := backends.WithExaminedCounts(context.Background())
		res, err := c.DistinctScan(ctx, &backends.DistinctParams{Key: "grp", Filter: filter})
		if err != nil {
			t.Fatal(err)
		}
		return res.Values, examined
	}

	values, examined := scan(nil)
	// The seed document's missing grp, 0..999, two numbers, two subdocuments.
	if len(values) != 1005 {
		t.Fatalf("%d values, want 1005", len(values))
	}
	if examined.Keys() > 1006 || examined.Docs() != 2 {
		t.Errorf("read %d keys and %d docs, want about one key per value and only the subdocuments", examined.Keys(), examined.Docs())
	}
	var sawInt64, sawDouble bool
	for _, v := range values {
		sawInt64 = sawInt64 || v == int64(5000)
		sawDouble = sawDouble || v == 5000.5
	}
	if !sawInt64 || !sawDouble {
		t.Errorf("values lost their types: int64 %v, double %v", sawInt64, sawDouble)
	}

	values, examined = scan(must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$lt", int32(100))))))
	if len(values) != 100 || examined.Keys() > 101 || examined.Docs() != 0 {
		t.Errorf("filtered: %d values from %d keys and %d docs, want 100 from about 100 keys and none", len(values), examined.Keys(), examined.Docs())
	}
}

// TestDistinctScanOfBuiltIndex covers an index built over existing documents,
// whose last seek starts past its last entry.
func TestDistinctScanOfBuiltIndex(t *testing.T) {
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "other", Key: []backends.IndexKeyPair{{Field: "zzz"}}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	var docs []*types.Document
	for i := int64(1); i <= 1000; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", i%10, "payload", string(make([]byte, 900)))))
	}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndexes(context.Background(), &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}}}}); err != nil {
		t.Fatal(err)
	}
	res, err := c.DistinctScan(context.Background(), &backends.DistinctParams{Key: "grp"})
	if err != nil {
		t.Fatal(err)
	}
	// The seed document's missing grp, and 0..9.
	if len(res.Values) != 11 {
		t.Errorf("values %v, want null and 0..9", res.Values)
	}
}
