// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bsonindexed

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

func TestLookupTopLevelScalar(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "a", int32(123), "b", "hello")
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	r, err := idx.Lookup(ctx, "a")
	if err != nil {
		t.Fatalf("Lookup(a): %v", err)
	}
	if !r.Found {
		t.Fatalf("Lookup(a) not found")
	}
	if r.TypeByte != typeInt32 {
		t.Errorf("Lookup(a) type = 0x%02x; want 0x%02x", r.TypeByte, typeInt32)
	}
	if got := int32(binary.LittleEndian.Uint32(r.Value)); got != 123 {
		t.Errorf("Lookup(a) value = %d; want 123", got)
	}

	r, err = idx.Lookup(ctx, "b")
	if err != nil {
		t.Fatalf("Lookup(b): %v", err)
	}
	if !r.Found {
		t.Fatalf("Lookup(b) not found")
	}
	if r.TypeByte != typeString {
		t.Errorf("Lookup(b) type = 0x%02x; want 0x%02x", r.TypeByte, typeString)
	}
}

func TestLookupMissing(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "a", int32(1))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	r, err := idx.Lookup(ctx, "missing")
	if err != nil {
		t.Fatalf("Lookup(missing): %v", err)
	}
	if r.Found {
		t.Errorf("Lookup(missing) reported found")
	}
}

func TestLookupNestedObject(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	inner, err := wirebson.NewDocument("x", int32(42), "y", "deep")
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	buf := encodeBSON(t, "wrap", inner)
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	r, err := idx.Lookup(ctx, "wrap.x")
	if err != nil {
		t.Fatalf("Lookup(wrap.x): %v", err)
	}
	if !r.Found || r.TypeByte != typeInt32 {
		t.Fatalf("Lookup(wrap.x) = %+v; want found int32", r)
	}
	if got := int32(binary.LittleEndian.Uint32(r.Value)); got != 42 {
		t.Errorf("wrap.x value = %d; want 42", got)
	}

	r, err = idx.Lookup(ctx, "wrap.y")
	if err != nil {
		t.Fatalf("Lookup(wrap.y): %v", err)
	}
	if !r.Found {
		t.Fatalf("Lookup(wrap.y) not found")
	}
	// String value is length-prefixed: 4-byte LE length + bytes + 0x00.
	strLen := int(binary.LittleEndian.Uint32(r.Value))
	if strLen != 5 { // "deep" + NUL terminator
		t.Errorf("string length = %d; want 5", strLen)
	}
	if !bytes.Equal(r.Value[4:4+strLen-1], []byte("deep")) {
		t.Errorf("string content = %q; want \"deep\"", r.Value[4:4+strLen-1])
	}
}

func TestLookupArrayElement(t *testing.T) {
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

	r, err := idx.Lookup(ctx, "items.1")
	if err != nil {
		t.Fatalf("Lookup(items.1): %v", err)
	}
	if !r.Found || r.TypeByte != typeInt32 {
		t.Fatalf("Lookup(items.1) = %+v; want found int32", r)
	}
	if got := int32(binary.LittleEndian.Uint32(r.Value)); got != 20 {
		t.Errorf("items.1 value = %d; want 20", got)
	}
}

func TestLookupOutOfBoundsArrayIndex(t *testing.T) {
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

	r, err := idx.Lookup(ctx, "items.5")
	if err != nil {
		t.Fatalf("Lookup(items.5): %v", err)
	}
	if r.Found {
		t.Errorf("items.5 reported found in a 2-element array")
	}
}

func TestLookupHandlesDollarPrefix(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	buf := encodeBSON(t, "field", int32(99))
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	r, err := idx.Lookup(ctx, "$.field")
	if err != nil {
		t.Fatalf("Lookup($.field): %v", err)
	}
	if !r.Found {
		t.Errorf("Lookup($.field) not found")
	}
}

func TestHas(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	inner, err := wirebson.NewDocument("inner_field", "x")
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	buf := encodeBSON(t, "a", int32(1), "b", inner)
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	for _, c := range []struct {
		path string
		want bool
	}{
		{"a", true},
		{"b", true},
		{"b.inner_field", true},
		{"b.nope", false},
		{"missing", false},
	} {
		got, err := idx.Has(ctx, c.path)
		if err != nil {
			t.Errorf("Has(%q): %v", c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("Has(%q) = %v; want %v", c.path, got, c.want)
		}
	}
}

func TestLookupOnLargeChunkedDoc(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	// 40 fields with 200-byte payload each forces multi-chunk
	// chunking; lookup must still find the target.
	fields := make([]any, 0, 80)
	for i := 0; i < 40; i++ {
		fields = append(fields, "f"+twoDigitDecimal(i), int32(i))
	}
	buf := encodeBSON(t, fields...)
	idx, err := Serialize(ctx, ns, buf)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	for i := 0; i < 40; i += 5 {
		path := "f" + twoDigitDecimal(i)
		r, err := idx.Lookup(ctx, path)
		if err != nil {
			t.Errorf("Lookup(%q): %v", path, err)
			continue
		}
		if !r.Found {
			t.Errorf("Lookup(%q): not found", path)
			continue
		}
		if got := int32(binary.LittleEndian.Uint32(r.Value)); got != int32(i) {
			t.Errorf("Lookup(%q) value = %d; want %d", path, got, i)
		}
	}
}

func twoDigitDecimal(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
