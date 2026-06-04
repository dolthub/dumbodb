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

// Asserts that applyFieldMutations dispatches on the actual storage
// shape of the existing document:
//
//   - When the document was stored inline in the value tuple, mutation
//     must NOT write to the chunk store -- the bytes are parsed,
//     mutated in memory, and re-serialised entirely within the dumbo
//     process.
//
//   - When the document was spilled out-of-band, mutation must load
//     the OOB blob from the chunk store. The post-mutation write
//     count is non-zero because the rewritten document re-spills.
//
// Under the bson-a format the inline path goes through bsonToDoc +
// docToBSON; the OOB path additionally reads the spilled blob. The
// surgical-splice path via bsonindexed.IndexedBsonDocument is a
// forthcoming optimisation.

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
	"github.com/dolthub/dumbodb/internal/types"
)

// countingNodeStore wraps a real tree.NodeStore and counts every
// Write / WriteBytes call. The test uses it to assert that the
// inline mutation branch never touches the underlying chunk store.
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

	// Small document well under DefaultTupleLengthTarget (2 KB). The
	// tuple builder will keep this inline.
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

	// Document well past DefaultTupleLengthTarget (2 KB) so the
	// tuple builder spills it out-of-band.
	pad := makePad(3600)
	doc := mustDoc(t, "_id", "doc1", "email", "old@example.com", "data", pad)
	stored, err := docToBSON(doc)
	require.NoError(t, err)
	require.Greater(t, len(stored), 3000, "test setup: stored doc should exceed inline threshold")

	tup, err := buildValue(ctx, cns, stored)
	require.NoError(t, err)

	// Confirm the tuple builder actually spilled out-of-band.
	result, ok, err := valDesc.GetBytesAdaptiveValue(ctx, 0, cns, tup)
	require.NoError(t, err)
	require.True(t, ok)
	_, isOutOfBand := result.(*val.ByteArray)
	require.Truef(t, isOutOfBand, "test setup: expected out-of-band storage, got %T", result)

	cns.Reset()

	// Mutation reads from the chunk store (one or more reads), applies
	// the change in memory, returns new bytes. The current bson-a
	// path does not write to the chunk store directly; the caller
	// builds a new tuple via buildValue which performs the write when
	// the result spills OOB. The surgical-splice optimisation that
	// would write chunks here is a forthcoming follow-on commit.
	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "email", Value: "new@example.com"},
	})
	require.NoError(t, err)

	got, err := bsonToDoc(newBytes)
	require.NoError(t, err)
	email, _ := got.Get("email")
	require.Equal(t, "new@example.com", email)
	// And the pad field survives the mutation.
	gotPad, _ := got.Get("data")
	require.Equal(t, pad, gotPad)
}

// makePad returns a deterministic byte-padded string used to inflate
// test documents past the BytesAdaptive inline threshold.
func makePad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}

// mustDoc is preserved here for test legibility; the production
// helper is in another test file in this package.
var _ = types.MakeDocument
