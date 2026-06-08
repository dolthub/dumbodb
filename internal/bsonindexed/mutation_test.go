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
	"context"
	"encoding/binary"
	"testing"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

func int32Value(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}

func stringValue(s string) []byte {
	out := make([]byte, 4+len(s)+1)
	binary.LittleEndian.PutUint32(out, uint32(len(s)+1))
	copy(out[4:], s)
	out[len(out)-1] = 0x00
	return out
}

func TestSetExistingField(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "a", int32(1), "b", int32(2))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.SetField(ctx, "a", typeInt32, int32Value(99))
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}

	r, _ := idx2.Lookup(ctx, "a")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 99 {
		t.Errorf("a after set = %+v; want int32(99)", r)
	}
	r, _ = idx2.Lookup(ctx, "b")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 2 {
		t.Errorf("b after set on a = %+v; want int32(2)", r)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestSetNewFieldKeepsLexOrder(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "a", int32(1), "c", int32(3))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.SetField(ctx, "b", typeInt32, int32Value(2))
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}
	all, err := idx2.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	rd, err := wirebson.RawDocument(all).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make([]string, 0, 3)
	for k := range rd.All() {
		got = append(got, k)
	}
	want := []string{"a", "b", "c"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("fields after insert = %v; want %v", got, want)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestUnsetField(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "a", int32(1), "b", int32(2), "c", int32(3))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.UnsetField(ctx, "b")
	if err != nil {
		t.Fatalf("UnsetField: %v", err)
	}
	has, err := idx2.Has(ctx, "b")
	if err != nil {
		t.Fatalf("Has(b): %v", err)
	}
	if has {
		t.Errorf("b still present after unset")
	}
	for _, k := range []string{"a", "c"} {
		ok, err := idx2.Has(ctx, k)
		if err != nil {
			t.Fatalf("Has(%s): %v", k, err)
		}
		if !ok {
			t.Errorf("%s missing after unset(b)", k)
		}
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestSetNestedFieldPatchesAncestorLengths(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	inner, err := wirebson.NewDocument("x", int32(1), "y", int32(2))
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	buf := encodeBSON(t, "outer", inner, "tail", int32(99))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.SetField(ctx, "outer.z", typeString, stringValue("a much longer string value"))
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}
	r, err := idx2.Lookup(ctx, "outer.z")
	if err != nil {
		t.Fatalf("Lookup(outer.z): %v", err)
	}
	if !r.Found {
		t.Fatalf("outer.z not found after insert")
	}
	r, _ = idx2.Lookup(ctx, "outer.x")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 1 {
		t.Errorf("outer.x after insert outer.z = %+v; want int32(1)", r)
	}
	r, _ = idx2.Lookup(ctx, "tail")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 99 {
		t.Errorf("tail after insert outer.z = %+v; want int32(99)", r)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestPushArray(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	arr, err := wirebson.NewArray(int32(10), int32(20))
	if err != nil {
		t.Fatalf("arr: %v", err)
	}
	buf := encodeBSON(t, "items", arr)
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.PushArray(ctx, "items", typeInt32, int32Value(30))
	if err != nil {
		t.Fatalf("PushArray: %v", err)
	}
	r, _ := idx2.Lookup(ctx, "items.2")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 30 {
		t.Errorf("items.2 after push = %+v; want int32(30)", r)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestPopArray(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	arr, err := wirebson.NewArray(int32(10), int32(20), int32(30))
	if err != nil {
		t.Fatalf("arr: %v", err)
	}
	buf := encodeBSON(t, "items", arr)
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	idx2, err := idx.PopArray(ctx, "items")
	if err != nil {
		t.Fatalf("PopArray: %v", err)
	}
	has, _ := idx2.Has(ctx, "items.2")
	if has {
		t.Errorf("items.2 still present after pop")
	}
	r, _ := idx2.Lookup(ctx, "items.1")
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 20 {
		t.Errorf("items.1 after pop = %+v; want int32(20)", r)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

func TestDeepNestedMutation(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	d5, err := wirebson.NewDocument("e", int32(1))
	if err != nil {
		t.Fatalf("d5: %v", err)
	}
	d4, err := wirebson.NewDocument("d", d5)
	if err != nil {
		t.Fatalf("d4: %v", err)
	}
	d3, err := wirebson.NewDocument("c", d4)
	if err != nil {
		t.Fatalf("d3: %v", err)
	}
	d2, err := wirebson.NewDocument("b", d3)
	if err != nil {
		t.Fatalf("d2: %v", err)
	}
	buf := encodeBSON(t, "a", d2)

	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	idx2, err := idx.SetField(ctx, "a.b.c.d.e", typeInt32, int32Value(42))
	if err != nil {
		t.Fatalf("SetField deep: %v", err)
	}
	r, err := idx2.Lookup(ctx, "a.b.c.d.e")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !r.Found || int32(binary.LittleEndian.Uint32(r.Value)) != 42 {
		t.Errorf("a.b.c.d.e after set = %+v; want int32(42)", r)
	}
	verifyDocLengthPrefix(t, ctx, idx2)
}

// verifyDocLengthPrefix is the cheap canary for ancestor-length
// patching bugs.
func verifyDocLengthPrefix(t *testing.T, ctx context.Context, idx IndexedBsonDocument) {
	t.Helper()
	buf, err := idx.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	declared := int(binary.LittleEndian.Uint32(buf))
	if declared != len(buf) {
		t.Errorf("root length prefix = %d; actual buffer length = %d", declared, len(buf))
	}
	if buf[len(buf)-1] != 0x00 {
		t.Errorf("root trailing byte = 0x%02x; want 0x00", buf[len(buf)-1])
	}
}
