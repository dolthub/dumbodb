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

package types

import (
	"bytes"
	"testing"

	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestSymbolJavaScriptEncoding(t *testing.T) {
	doc := must.NotFail(NewDocument("s", Symbol("ab"), "j", JavaScript("c")))
	want := []byte{
		0x18, 0, 0, 0,
		0x0e, 's', 0x00, 0x03, 0, 0, 0, 'a', 'b', 0x00,
		0x0d, 'j', 0x00, 0x02, 0, 0, 0, 'c', 0x00,
		0x00,
	}
	if got := doc.AppendBSON(nil); !bytes.Equal(got, want) {
		t.Fatalf("AppendBSON = % x, want % x", got, want)
	}
	if got := doc.AppendBSONSortedKeys(nil); !bytes.Equal(got, sortedSymbolJavaScriptBSON) {
		t.Fatalf("AppendBSONSortedKeys = % x, want % x", got, sortedSymbolJavaScriptBSON)
	}
	if doc.BSONSize() != len(want) {
		t.Fatalf("BSONSize = %d, want %d", doc.BSONSize(), len(want))
	}
}

var sortedSymbolJavaScriptBSON = []byte{
	0x18, 0, 0, 0,
	0x0d, 'j', 0x00, 0x02, 0, 0, 0, 'c', 0x00,
	0x0e, 's', 0x00, 0x03, 0, 0, 0, 'a', 'b', 0x00,
	0x00,
}

func TestSymbolComparesAsString(t *testing.T) {
	for _, tc := range []struct {
		a, b any
		want CompareResult
	}{
		{Symbol("abc"), "abc", Equal},
		{"abc", Symbol("abc"), Equal},
		{Symbol("abc"), Symbol("abd"), Less},
		{"abd", Symbol("abc"), Greater},
		{Symbol("a"), int32(1), Greater},
		{Symbol("a"), must.NotFail(NewDocument()), Less},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if Identical(Symbol("abc"), "abc") {
		t.Error(`Symbol("abc") must not be identical to "abc"`)
	}
	if !Identical(Symbol("abc"), Symbol("abc")) {
		t.Error(`Symbol("abc") must be identical to itself`)
	}
}

func TestJavaScriptOrdering(t *testing.T) {
	if Compare(JavaScript("a"), "a") == Equal {
		t.Error("JavaScript must not equal a string")
	}
	if Compare(JavaScript("a"), JavaScript("b")) != Less {
		t.Error("JavaScript values compare by content")
	}
	for _, tc := range []struct {
		lo, hi any
	}{
		{"z", JavaScript("a")},
		{Symbol("z"), JavaScript("a")},
		{Regex{Pattern: "z"}, JavaScript("a")},
		{Timestamp(1), JavaScript("a")},
		{JavaScript("z"), MaxKey},
	} {
		if CompareOrder(tc.lo, tc.hi, Ascending) != Less {
			t.Errorf("%v must sort before %v", tc.lo, tc.hi)
		}
	}
	if Identical(JavaScript("a"), "a") || !Identical(JavaScript("a"), JavaScript("a")) {
		t.Error("JavaScript identity must include its type")
	}
}

func TestSymbolJavaScriptDeepCopyAndFormat(t *testing.T) {
	doc := must.NotFail(NewDocument("s", Symbol("x"), "j", JavaScript("y")))
	copied := doc.DeepCopy()
	if v, _ := copied.Get("s"); v != Symbol("x") {
		t.Fatalf("copied s = %#v", v)
	}
	if got := FormatAnyValue(doc); got != `{ s: Symbol("x"), j: Code("y") }` {
		t.Fatalf("FormatAnyValue = %s", got)
	}
}
