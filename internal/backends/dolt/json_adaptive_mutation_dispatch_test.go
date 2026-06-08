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

// Asserts applyFieldMutations dispatches on storage shape: inline
// mutations write 0 chunks; OOB mutations read the blob and re-spill.

package dolt

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

type countingNodeStore struct {
	tree.NodeStore
	writes int64
}

func (c *countingNodeStore) Write(ctx context.Context, n *tree.Node) (hash.Hash, error) {
	atomic.AddInt64(&c.writes, 1)
	return c.NodeStore.Write(ctx, n)
}

func (c *countingNodeStore) WriteBytes(ctx context.Context, b []byte) (hash.Hash, error) {
	atomic.AddInt64(&c.writes, 1)
	return c.NodeStore.WriteBytes(ctx, b)
}

func (c *countingNodeStore) Writes() int64 {
	return atomic.LoadInt64(&c.writes)
}

func (c *countingNodeStore) Reset() {
	atomic.StoreInt64(&c.writes, 0)
}

// realNodeStore returns a usable NodeStore from a transient test
// backend. The backend's collection storage gets touched as a side
// effect of forcing state initialisation, which the per-test cleanup
// in newTestBackend handles.
func realNodeStore(t *testing.T) tree.NodeStore {
	t.Helper()
	b := newTestBackend(t)
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1)))
	state, err := b.getOrOpenDB(context.Background(), "testdb", false)
	require.NoError(t, err)
	return state.ns
}

func TestApplyFieldMutations_InlineDoesNotWriteToChunkStore(t *testing.T) {
	ctx := context.Background()
	cns := &countingNodeStore{NodeStore: realNodeStore(t)}

	doc := mustDoc(t, "_id", "doc1", "email", "old@example.com", "age", int32(30))
	stored, err := docToBSON(doc)
	require.NoError(t, err)
	tup, err := buildValue(ctx, cns, stored)
	require.NoError(t, err)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "email", Value: "new@example.com"},
	})
	require.NoError(t, err)

	require.Equalf(t, int64(0), cns.Writes(),
		"inline mutation must not touch chunk store; got %d writes", cns.Writes())
	got, err := bsonToDoc(newBytes)
	require.NoError(t, err)
	email, _ := got.Get("email")
	require.Equal(t, "new@example.com", email)
}

func TestApplyFieldMutations_InlineHandlesUnset(t *testing.T) {
	ctx := context.Background()
	cns := &countingNodeStore{NodeStore: realNodeStore(t)}

	doc := mustDoc(t, "_id", "doc1", "email", "old@example.com", "disposable", "x")
	stored, err := docToBSON(doc)
	require.NoError(t, err)
	tup, err := buildValue(ctx, cns, stored)
	require.NoError(t, err)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "disposable", Unset: true},
	})
	require.NoError(t, err)

	require.Equal(t, int64(0), cns.Writes(), "inline $unset must not touch chunk store")
	got, err := bsonToDoc(newBytes)
	require.NoError(t, err)
	_, err = got.Get("disposable")
	require.Error(t, err, "disposable field should be removed")
	email, _ := got.Get("email")
	require.Equal(t, "old@example.com", email)
}

func TestApplyFieldMutations_OutOfBandRoundTrip(t *testing.T) {
	ctx := context.Background()
	cns := &countingNodeStore{NodeStore: realNodeStore(t)}

	pad := makePad(3600)
	doc := mustDoc(t, "_id", "doc1", "email", "old@example.com", "data", pad)
	stored, err := docToBSON(doc)
	require.NoError(t, err)
	require.Greater(t, len(stored), 3000, "test setup: stored doc should exceed inline threshold")

	tup, err := buildValue(ctx, cns, stored)
	require.NoError(t, err)

	result, ok, err := valDesc.GetBytesAdaptiveValue(ctx, 0, cns, tup)
	require.NoError(t, err)
	require.True(t, ok)
	_, isOutOfBand := result.(*val.ByteArray)
	require.Truef(t, isOutOfBand, "test setup: expected out-of-band storage, got %T", result)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "email", Value: "new@example.com"},
	})
	require.NoError(t, err)

	got, err := bsonToDoc(newBytes)
	require.NoError(t, err)
	email, _ := got.Get("email")
	require.Equal(t, "new@example.com", email)
	gotPad, _ := got.Get("data")
	require.Equal(t, pad, gotPad)
}

func makePad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}

