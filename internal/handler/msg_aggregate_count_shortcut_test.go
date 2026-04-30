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

func doc(pairs ...any) *types.Document {
	return must.NotFail(types.NewDocument(pairs...))
}

func TestTryCountAggregateShortcut(t *testing.T) {
	t.Parallel()

	// Mirrors the exact pipeline emitted by mongo-driver/v2 CountDocuments:
	// [{$match: {}}, {$group: {_id: 1, n: {$sum: 1}}}]
	driverV2Pipeline := []any{
		doc("$match", doc()),
		doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
	}

	tests := map[string]struct {
		stages   []any
		want     bool
		wantOut  string
		wantSkip int64
		wantLim  int64
	}{
		"go driver v2 default": {
			stages:  driverV2Pipeline,
			want:    true,
			wantOut: "n",
			wantLim: -1,
		},
		"with skip and limit": {
			stages: []any{
				doc("$match", doc()),
				doc("$skip", int64(5)),
				doc("$limit", int64(10)),
				doc("$group", doc("_id", types.Null, "n", doc("$sum", int32(1)))),
			},
			want:     true,
			wantOut:  "n",
			wantSkip: 5,
			wantLim:  10,
		},
		"id null literal": {
			stages: []any{
				doc("$match", doc()),
				doc("$group", doc("_id", types.Null, "count", doc("$sum", int32(1)))),
			},
			want:    true,
			wantOut: "count",
			wantLim: -1,
		},
		"non empty match rejects": {
			stages: []any{
				doc("$match", doc("a", int32(1))),
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"sum 2 rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(2)))),
			},
			want: false,
		},
		"id field reference rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$group", doc("_id", "$user", "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"id document rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$group", doc("_id", doc("a", int32(1)), "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"extra group field rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$group", doc(
					"_id", int32(1),
					"n", doc("$sum", int32(1)),
					"x", doc("$sum", int32(1)),
				)),
			},
			want: false,
		},
		"unknown stage rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$sort", doc("a", int32(1))),
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"missing group rejects": {
			stages: []any{
				doc("$match", doc()),
			},
			want: false,
		},
		"empty pipeline rejects": {
			stages: nil,
			want:   false,
		},
		"match not first rejects": {
			stages: []any{
				doc("$skip", int64(1)),
				doc("$match", doc()),
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"negative skip rejects": {
			stages: []any{
				doc("$match", doc()),
				doc("$skip", int64(-1)),
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
			},
			want: false,
		},
		"group only no match": {
			stages: []any{
				doc("$group", doc("_id", int32(1), "n", doc("$sum", int32(1)))),
			},
			want:    true,
			wantOut: "n",
			wantLim: -1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			info, ok := tryCountAggregateShortcut(tc.stages)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if info.outField != tc.wantOut {
				t.Errorf("outField = %q, want %q", info.outField, tc.wantOut)
			}
			if info.skip != tc.wantSkip {
				t.Errorf("skip = %d, want %d", info.skip, tc.wantSkip)
			}
			if info.limit != tc.wantLim {
				t.Errorf("limit = %d, want %d", info.limit, tc.wantLim)
			}
		})
	}
}
