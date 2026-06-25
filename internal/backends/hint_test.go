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

package backends

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A key-pattern hint must match the index key direction: a {a:-1} hint must
// not resolve to an {a:1} index (MongoDB exact key-pattern requirement).
func TestMatchHintedIndex_KeyPatternDirection(t *testing.T) {
	asc := []IndexInfo{{Name: "a_1", Key: []IndexKeyPair{{Field: "a", Descending: false}}}}
	desc := []IndexInfo{{Name: "a_-1", Key: []IndexKeyPair{{Field: "a", Descending: true}}}}

	doc := func(dir int32) *types.Document { return must.NotFail(types.NewDocument("a", dir)) }

	cases := []struct {
		name string
		hint any
		idx  []IndexInfo
		want string
	}{
		{"asc hint, asc index", doc(1), asc, "a_1"},
		{"desc hint, asc index (no match)", doc(-1), asc, ""},
		{"desc hint, desc index", doc(-1), desc, "a_-1"},
		{"asc hint, desc index (no match)", doc(1), desc, ""},
		{"name hint ignores direction", "a_1", asc, "a_1"},
	}
	for _, tc := range cases {
		if got := MatchHintedIndex(tc.hint, tc.idx); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
