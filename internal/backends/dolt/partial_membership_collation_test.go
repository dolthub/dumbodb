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
	"math"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A collated partial index holds the documents its filter expression matches
// under the index's collation.
func TestPartialFilterMembershipCollation(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	pfe := doc("a", doc("$gte", "m"))
	en2 := doc("locale", "en", "strength", int32(2))
	for _, tc := range []struct {
		name               string
		a                  any
		collated, bytewise bool
	}{
		{"case variant", "N", true, false},
		{"lowercase", "n", true, true},
		{"capital after m", "Zed", true, false},
		{"array with a case variant", arr(math.NaN(), "N", int32(1)), true, false},
		{"below m", "a", false, false},
	} {
		d := doc("_id", int32(1), "a", tc.a)
		if got := must.NotFail(backends.PartialFilterMembership(pfe, en2)(d)); got != tc.collated {
			t.Errorf("%s under en/2: member %v, want %v", tc.name, got, tc.collated)
		}
		if got := must.NotFail(backends.PartialFilterMembership(pfe, nil)(d)); got != tc.bytewise {
			t.Errorf("%s bytewise: member %v, want %v", tc.name, got, tc.bytewise)
		}
	}
}
