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

package handler

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCountingPipeline(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	match := doc("$match", doc("grp", int32(5)))
	countGroup := doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1))))
	project := doc("$project", doc("_id", false))
	for _, tc := range []struct {
		name     string
		pipeline []any
		ok       bool
		rest     int
	}{
		{"countDocuments", []any{match, countGroup}, true, 0},
		{"no $match", []any{countGroup}, true, 0},
		{"$count", []any{match, doc("$count", "n")}, true, 1},
		{"stages after the count", []any{match, countGroup, project}, true, 1},
		{"group by a field", []any{match, doc("$group", doc("_id", "$grp"))}, false, 0},
		{"$limit before the count", []any{match, doc("$limit", int32(5)), countGroup}, false, 0},
		{"$lookup after the count", []any{match, countGroup, doc("$lookup", doc("from", "x"))}, false, 0},
		{"no count", []any{match}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, ok := countingPipeline(tc.pipeline)
			if ok != tc.ok || len(rest) != tc.rest {
				t.Errorf("countingPipeline = %d stages after, %v; want %d, %v", len(rest), ok, tc.rest, tc.ok)
			}
		})
	}
}
