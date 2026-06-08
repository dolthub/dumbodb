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

package bsonindexed

import (
	"bytes"
	"sort"
	"testing"
)

func TestRootLocation(t *testing.T) {
	loc := NewRootLocation()
	if loc.Size() != 0 {
		t.Errorf("root size = %d; want 0", loc.Size())
	}
	if loc.State() != StartOfValue {
		t.Errorf("root state = %v; want StartOfValue", loc.State())
	}
	if got := loc.ToMongoPath(); got != "" {
		t.Errorf("root path = %q; want empty", got)
	}
}

func TestAppendObjectKey(t *testing.T) {
	loc := NewRootLocation()
	loc.AppendObjectKey([]byte("a"))
	loc.AppendObjectKey([]byte("b"))
	loc.AppendObjectKey([]byte("c"))
	if loc.Size() != 3 {
		t.Fatalf("size = %d; want 3", loc.Size())
	}
	if got := loc.ToMongoPath(); got != "a.b.c" {
		t.Errorf("path = %q; want %q", got, "a.b.c")
	}
	if el := loc.PathElement(1); el.IsArrayIndex || string(el.Key) != "b" {
		t.Errorf("element[1] = %+v; want object key 'b'", el)
	}
}

func TestAppendArrayIndex(t *testing.T) {
	loc := NewRootLocation()
	loc.AppendObjectKey([]byte("arr"))
	loc.AppendArrayIndex(7)
	if loc.Size() != 2 {
		t.Fatalf("size = %d; want 2", loc.Size())
	}
	if got := loc.ToMongoPath(); got != "arr.7" {
		t.Errorf("path = %q; want %q", got, "arr.7")
	}
	el := loc.LastPathElement()
	if !el.IsArrayIndex || el.ArrayIndex() != 7 {
		t.Errorf("last element = %+v; want array index 7", el)
	}
}

func TestPop(t *testing.T) {
	loc := NewRootLocation()
	loc.AppendObjectKey([]byte("a"))
	loc.AppendObjectKey([]byte("b"))
	loc.AppendObjectKey([]byte("c"))
	loc.Pop()
	if loc.Size() != 2 {
		t.Fatalf("after pop size = %d; want 2", loc.Size())
	}
	if got := loc.ToMongoPath(); got != "a.b" {
		t.Errorf("after pop path = %q; want %q", got, "a.b")
	}
	loc.Pop()
	loc.Pop()
	if loc.Size() != 0 {
		t.Errorf("after triple pop size = %d; want 0", loc.Size())
	}
}

func TestFromMongoPath(t *testing.T) {
	cases := []struct {
		in     string
		wantSz int
		wantP  string
	}{
		{"", 0, ""},
		{"a", 1, "a"},
		{"a.b.c", 3, "a.b.c"},
		{"arr.5", 2, "arr.5"},
		{"a.0.b", 3, "a.0.b"},
		{"users.42.name", 3, "users.42.name"},
	}
	for _, c := range cases {
		loc, err := FromMongoPath(c.in)
		if err != nil {
			t.Errorf("FromMongoPath(%q): %v", c.in, err)
			continue
		}
		if loc.Size() != c.wantSz {
			t.Errorf("FromMongoPath(%q).Size() = %d; want %d", c.in, loc.Size(), c.wantSz)
		}
		if got := loc.ToMongoPath(); got != c.wantP {
			t.Errorf("FromMongoPath(%q).ToMongoPath() = %q; want %q", c.in, got, c.wantP)
		}
	}
}

func TestFromMongoPath_EmptyComponent(t *testing.T) {
	if _, err := FromMongoPath("a..b"); err == nil {
		t.Errorf("FromMongoPath(\"a..b\"): want error, got nil")
	}
}

func TestFromKeyRoundTrip(t *testing.T) {
	orig := NewRootLocation()
	orig.AppendObjectKey([]byte("a"))
	orig.AppendArrayIndex(13)
	orig.AppendObjectKey([]byte("name"))
	orig.SetState(EndOfValue)

	rebuilt := FromKey(orig.Key())
	if rebuilt.Size() != orig.Size() {
		t.Fatalf("size mismatch: %d vs %d", rebuilt.Size(), orig.Size())
	}
	if !bytes.Equal(rebuilt.Key(), orig.Key()) {
		t.Errorf("key mismatch:\n  got %x\n want %x", rebuilt.Key(), orig.Key())
	}
	if rebuilt.PathElement(1).ArrayIndex() != 13 {
		t.Errorf("element[1] index = %d; want 13", rebuilt.PathElement(1).ArrayIndex())
	}
}

func TestCompareLexOrderMatchesTraversalOrder(t *testing.T) {
	build := func(setup func(l *Location)) Location {
		l := NewRootLocation()
		setup(&l)
		return l
	}
	ordered := []Location{
		build(func(l *Location) { l.SetState(StartOfValue) }),
		build(func(l *Location) { l.AppendObjectKey([]byte("a")); l.SetState(StartOfValue) }),
		build(func(l *Location) { l.AppendObjectKey([]byte("a")); l.SetState(EndOfValue) }),
		build(func(l *Location) { l.AppendObjectKey([]byte("b")); l.SetState(StartOfValue) }),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("b"))
			l.AppendObjectKey([]byte("x"))
			l.SetState(StartOfValue)
		}),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("b"))
			l.AppendObjectKey([]byte("x"))
			l.SetState(EndOfValue)
		}),
		build(func(l *Location) { l.AppendObjectKey([]byte("b")); l.SetState(EndOfValue) }),
		build(func(l *Location) { l.AppendObjectKey([]byte("c")); l.SetState(StartOfValue) }),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("c"))
			l.AppendArrayIndex(0)
			l.SetState(StartOfValue)
		}),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("c"))
			l.AppendArrayIndex(0)
			l.SetState(EndOfValue)
		}),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("c"))
			l.AppendArrayIndex(1)
			l.SetState(StartOfValue)
		}),
		build(func(l *Location) {
			l.AppendObjectKey([]byte("c"))
			l.AppendArrayIndex(1)
			l.SetState(EndOfValue)
		}),
		build(func(l *Location) { l.AppendObjectKey([]byte("c")); l.SetState(EndOfValue) }),
		build(func(l *Location) { l.SetState(EndOfValue) }),
	}
	keys := make([]LocationKey, len(ordered))
	for i, l := range ordered {
		keys[i] = l.KeyClone()
	}
	shuffled := make([]LocationKey, len(keys))
	copy(shuffled, keys)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	sort.SliceStable(shuffled, func(i, j int) bool {
		c, err := Compare(shuffled[i], shuffled[j])
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		return c < 0
	})
	for i := range keys {
		if !bytes.Equal(shuffled[i], keys[i]) {
			t.Errorf("position %d after sort: got %x; want %x", i, shuffled[i], keys[i])
		}
	}
}

func TestIsAncestor(t *testing.T) {
	mk := func(parts ...string) LocationKey {
		l := NewRootLocation()
		for _, p := range parts {
			l.AppendObjectKey([]byte(p))
		}
		return l.KeyClone()
	}
	cases := []struct {
		name      string
		full      LocationKey
		prefix    LocationKey
		ancestral bool
	}{
		{"a is ancestor of a.b", mk("a", "b"), mk("a"), true},
		{"a is ancestor of a.b.c", mk("a", "b", "c"), mk("a"), true},
		{"a is not ancestor of a (equal)", mk("a"), mk("a"), false},
		{"a is not ancestor of aa", mk("aa"), mk("a"), false},
		{"a.b is not ancestor of a", mk("a"), mk("a", "b"), false},
		{"root keys", LocationKey{byte(StartOfValue)}, LocationKey{byte(StartOfValue)}, false},
	}
	for _, c := range cases {
		if got := IsAncestor(c.full, c.prefix); got != c.ancestral {
			t.Errorf("%s: IsAncestor(full=%x, prefix=%x) = %v; want %v",
				c.name, c.full, c.prefix, got, c.ancestral)
		}
	}
}

func TestModifySameArray(t *testing.T) {
	mk := func(steps ...interface{}) LocationKey {
		l := NewRootLocation()
		for _, s := range steps {
			switch v := s.(type) {
			case string:
				l.AppendObjectKey([]byte(v))
			case int:
				l.AppendArrayIndex(uint64(v))
			}
		}
		return l.KeyClone()
	}
	if !ModifySameArray(mk("a", "arr", 0), mk("a", "arr", 1)) {
		t.Errorf("paths a.arr[0] and a.arr[1] should modify same array")
	}
	if ModifySameArray(mk("a", "b"), mk("a", "c")) {
		t.Errorf("paths a.b and a.c should not be reported as same-array")
	}
	if ModifySameArray(mk("a", "arr", 0), mk("b", "arr", 0)) {
		t.Errorf("paths a.arr[0] and b.arr[0] differ at first step; not same array")
	}
}

func TestPutReadUint32LE(t *testing.T) {
	buf := make([]byte, 4)
	for _, v := range []uint32{0, 1, 0xFFFF, 0x12345678, 0xFFFFFFFF} {
		PutUint32LE(buf, v)
		if got := ReadUint32LE(buf); got != v {
			t.Errorf("round trip %d: got %d", v, got)
		}
	}
}
