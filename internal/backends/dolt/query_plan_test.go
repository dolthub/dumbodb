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
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// planChain flattens s into "STAGE(index)" entries, depth first.
func planChain(s *explainStage) []string {
	if s == nil {
		return nil
	}
	entry := s.stage
	if s.indexName != "" {
		entry += "(" + s.indexName + ")"
	}
	out := []string{entry}
	out = append(out, planChain(s.input)...)
	for _, in := range s.inputs {
		out = append(out, planChain(in)...)
	}
	return out
}

// TestQueryReportsItsPlan checks the stage tree query returns names the scan
// that served the query, which explain renders.
func TestQueryReportsItsPlan(t *testing.T) {
	b, _ := newSortVsFilterBackend(t, 100)
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	eq := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

	for _, tc := range []struct {
		name   string
		params *backends.QueryParams
		want   []string
	}{
		{"_id beside an indexed field reads _id", &backends.QueryParams{Filter: eq("_id", int32(5), "p", int32(2))}, []string{"FETCH", "IXSCAN(_id_)"}},
		{"narrow range in a small collection", &backends.QueryParams{Filter: eq("q", eq("$gte", int32(10)))}, []string{"FETCH", "IXSCAN(q)"}},
		{"$or union", &backends.QueryParams{Filter: eq("$or", must.NotFail(types.NewArray(eq("k", int32(1)), eq("q", int32(2)))))},
			[]string{"FETCH", "OR", "IXSCAN(k)", "IXSCAN(q)"}},
		{"unindexed field", &backends.QueryParams{Filter: eq("z", int32(1))}, []string{"COLLSCAN"}},
		{"hint the filter does not bound", &backends.QueryParams{Filter: eq("z", int32(1)), Hint: "p"}, []string{"FETCH", "IXSCAN(p)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, plan, err := c.query(context.Background(), tc.params)
			if err != nil {
				t.Fatal(err)
			}
			res.Iter.Close()
			if got := planChain(plan); !slices.Equal(got, tc.want) {
				t.Errorf("plan %s, want %s", strings.Join(got, " > "), strings.Join(tc.want, " > "))
			}
		})
	}
}
