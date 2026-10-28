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
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestRegexLiteralPrefix(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    string
		ok      bool
	}{
		{"^abc", "abc", true},
		{"^abc.*", "abc", true},
		{"^abc$", "abc", true},
		{"^ab+c", "ab", true},
		{"^abc?", "ab", true},
		{"^abc*", "ab", true},
		{"^abc{0,2}", "ab", true},
		{`^a\.b`, "a.b", true},
		{`^a\.?`, "a", true},
		{`^ab\d`, "ab", true},
		{"^ab(c)", "ab", true},
		{"^ab[cd]", "ab", true},
		{"^\303\251t\303\251", "\303\251t\303\251", true},
		{"abc", "", false},
		{"^", "", false},
		{"^.abc", "", false},
		{"^a?", "", false},
		{"^ab|cd", "", false},
		{`^\d`, "", false},
	} {
		got, ok := regexLiteralPrefix(tc.pattern)
		if got != tc.want || ok != tc.ok {
			t.Errorf("regexLiteralPrefix(%q) = %q, %v; want %q, %v", tc.pattern, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRegexPrefixRangesDeclines(t *testing.T) {
	for _, v := range []any{
		types.Regex{Pattern: "^abc", Options: "i"},
		types.Regex{Pattern: "^abc", Options: "m"},
		types.Regex{Pattern: "^abc", Options: "x"},
		types.Regex{Pattern: "^abc", Options: "q"},
		types.Regex{Pattern: "^abc("},
		must.NotFail(types.NewDocument("$regex", "^abc", "$options", "i")),
		must.NotFail(types.NewDocument("$regex", types.Regex{Pattern: "^abc", Options: "s"}, "$options", "s")),
		must.NotFail(types.NewDocument("$regex", "^abc", "$ne", "abcd")),
		must.NotFail(types.NewDocument("$regex", int32(1))),
		"^abc",
	} {
		if _, ok := regexPrefixRanges(v); ok {
			t.Errorf("regexPrefixRanges(%v) must decline", v)
		}
	}
	for _, v := range []any{
		types.Regex{Pattern: "^abc"},
		types.Regex{Pattern: "^abc", Options: "s"},
		must.NotFail(types.NewDocument("$regex", "^abc")),
		must.NotFail(types.NewDocument("$regex", "^abc", "$options", "s")),
	} {
		if _, ok := regexPrefixRanges(v); !ok {
			t.Errorf("regexPrefixRanges(%v) must give ranges", v)
		}
	}
}

func TestRegexPrefixReadsOnlyThePrefixRange(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "s", Key: []backends.IndexKeyPair{{Field: "s"}}})
	var docs []*types.Document
	for i := int32(1); i <= 100; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "s", fmt.Sprintf("s%03d", i))))
	}
	docs = append(docs,
		must.NotFail(types.NewDocument("_id", int32(101), "s", types.Regex{Pattern: "^s05"})),
		must.NotFail(types.NewDocument("_id", int32(102), "s", must.NotFail(types.NewArray("x", "s050x")))),
		must.NotFail(types.NewDocument("_id", int32(103), "s", "s05\x00z")),
	)
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("s", types.Regex{Pattern: "^s05"}))})
	slices.Sort(ids)
	want := []int32{50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 101, 102, 103}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}
