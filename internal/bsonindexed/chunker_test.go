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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

func TestSerializeRoundTripSmall(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	orig := encodeBSON(t, "a", int32(1), "b", "hello", "c", int64(42))

	idx, err := Serialize(ctx, ns, orig)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := idx.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("round trip mismatch:\n  got  %x\n  want %x", got, orig)
	}
}

func TestSerializeRoundTripNested(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	inner, err := wirebson.NewDocument("x", int32(1), "y", int32(2))
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	arr, err := wirebson.NewArray(int32(100), int32(200), int32(300))
	if err != nil {
		t.Fatalf("arr: %v", err)
	}
	orig := encodeBSON(t, "outer", inner, "items", arr, "tail", "end")

	idx, err := Serialize(ctx, ns, orig)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := idx.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("nested round trip mismatch:\n  got  %x\n  want %x", got, orig)
	}
}

func TestSerializeRoundTripLargeMultiChunk(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()

	// Construct a document larger than MinChunkSize so the chunker
	// has a chance to introduce intermediate leaves. Each "f<N>"
	// field carries a payload string that pads the doc out.
	fields := make([]any, 0, 80)
	for i := 0; i < 40; i++ {
		fields = append(fields, fmt.Sprintf("f%03d", i), strings.Repeat("x", 200))
	}
	orig := encodeBSON(t, fields...)
	if len(orig) < MinChunkSize+1024 {
		t.Fatalf("test fixture too small: %d bytes", len(orig))
	}

	idx, err := Serialize(ctx, ns, orig)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := idx.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("large round trip mismatch (%d bytes)", len(orig))
	}

	count, err := idx.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	// We expect at least 2 chunks for a doc larger than MinChunkSize;
	// exact count depends on the content-defined boundary placement.
	if count < 2 {
		t.Errorf("expected >=2 chunks for %d-byte doc, got %d", len(orig), count)
	}
	t.Logf("chunk count for %d-byte doc: %d", len(orig), count)
}

func TestSerializeRoundTripEmpty(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	orig := encodeBSON(t) // empty document: 5 bytes (length + terminator)

	idx, err := Serialize(ctx, ns, orig)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := idx.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("empty round trip mismatch:\n  got  %x\n  want %x", got, orig)
	}
}

func TestOpenRehydratesIndex(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()
	fields := make([]any, 0, 60)
	for i := 0; i < 30; i++ {
		fields = append(fields, fmt.Sprintf("k%03d", i), strings.Repeat("y", 250))
	}
	orig := encodeBSON(t, fields...)

	idx, err := Serialize(ctx, ns, orig)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	root := idx.Root()

	// Forget the original handle, reopen via Open with just the root
	// hash and verify the byte-identical document re-emerges.
	reopened, err := Open(ctx, ns, root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := reopened.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes after Open: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("post-Open round trip mismatch")
	}
}
