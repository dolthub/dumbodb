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

package common

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestDecideWriteConcern(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		wc                any
		wantSkip          bool
		wantUnsatisfiable bool
		wantUnknownTag    bool
	}{
		{"nil", nil, false, false, false},
		{"typed nil doc", (*types.Document)(nil), false, false, false},
		{"empty doc (default w:1 j:implicit)", must.NotFail(types.NewDocument()), false, false, false},
		{"j:true explicit", must.NotFail(types.NewDocument("j", true)), false, false, false},
		{"j:false opts out of fsync", must.NotFail(types.NewDocument("j", false)), true, false, false},
		{"w:1", must.NotFail(types.NewDocument("w", int32(1))), false, false, false},
		{"w:2", must.NotFail(types.NewDocument("w", int32(2))), false, true, false},
		{"w:0 fire-and-forget", must.NotFail(types.NewDocument("w", int32(0))), true, false, false},
		{"w:0 (int64)", must.NotFail(types.NewDocument("w", int64(0))), true, false, false},
		{"w:0 (float64)", must.NotFail(types.NewDocument("w", float64(0))), true, false, false},
		{"w:majority string unchanged", must.NotFail(types.NewDocument("w", "majority")), false, false, false},
		{"custom tag", must.NotFail(types.NewDocument("w", "east")), false, false, true},
		{"wtimeout ignored", must.NotFail(types.NewDocument("w", int32(1), "wtimeout", int64(1000))), false, false, false},
		{"w:0 + j:true still skips (w wins by itself)", must.NotFail(types.NewDocument("w", int32(0), "j", true)), true, false, false},
		{"wrong shape (not a doc)", "not-a-doc", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := DecideWriteConcern(tc.wc)
			if got.SkipDurableSync != tc.wantSkip {
				t.Fatalf("SkipDurableSync = %v, want %v", got.SkipDurableSync, tc.wantSkip)
			}
			if got.Unsatisfiable != tc.wantUnsatisfiable {
				t.Fatalf("Unsatisfiable = %v, want %v", got.Unsatisfiable, tc.wantUnsatisfiable)
			}
			if got.UnknownTag != tc.wantUnknownTag {
				t.Fatalf("UnknownTag = %v, want %v", got.UnknownTag, tc.wantUnknownTag)
			}
		})
	}
}
