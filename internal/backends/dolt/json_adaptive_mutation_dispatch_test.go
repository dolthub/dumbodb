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
//     process. This matches Dolt's own adaptive-JSON UPDATE path.
//
//   - When the document was spilled out-of-band, mutation routes
//     through IndexedJsonDocument structural sharing, which DOES
//     touch the chunk store. (At least one Write is expected; the
//     exact count depends on how the new prolly tree shares chunks.)
//
// See workspace-a3u (this dispatch) and workspace-110 (the
// investigation that produced the design).

package dolt

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
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

	// Small ExtJSON document well under DefaultTupleLengthTarget (2 KB).
	// The tuple builder will keep this inline.
	inlineJSON := []byte(`{"_id":"doc1","email":"old@example.com","age":{"$numberInt":"30"}}`)
	tup, err := buildValue(ctx, cns, inlineJSON)
	require.NoError(t, err)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "email", Value: "new@example.com"},
	})
	require.NoError(t, err)

	require.Equalf(t, int64(0), cns.Writes(),
		"inline mutation must not touch chunk store; got %d writes", cns.Writes())
	require.Contains(t, string(newBytes), `"new@example.com"`,
		"new email value not present in mutated document")
	require.NotContains(t, string(newBytes), `"old@example.com"`,
		"old email value still present after $set")
}

func TestApplyFieldMutations_InlineHandlesUnset(t *testing.T) {
	ctx := context.Background()
	cns := &countingNodeStore{NodeStore: realNodeStore(t)}

	inlineJSON := []byte(`{"_id":"doc1","email":"old@example.com","disposable":"x"}`)
	tup, err := buildValue(ctx, cns, inlineJSON)
	require.NoError(t, err)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "disposable", Unset: true},
	})
	require.NoError(t, err)

	require.Equal(t, int64(0), cns.Writes(), "inline $unset must not touch chunk store")
	require.NotContains(t, string(newBytes), `"disposable"`)
	require.Contains(t, string(newBytes), `"old@example.com"`)
}

func TestApplyFieldMutations_OutOfBandUsesChunkStore(t *testing.T) {
	ctx := context.Background()
	cns := &countingNodeStore{NodeStore: realNodeStore(t)}

	// Document well past DefaultTupleLengthTarget (2 KB) so the
	// tuple builder spills it out-of-band.
	pad := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 100) // ~3.6 KB of varied chars
	largeJSON := []byte(fmt.Sprintf(`{"_id":"doc1","email":"old@example.com","data":%q}`, pad))
	require.Greater(t, len(largeJSON), 3000, "test setup: large JSON should exceed inline threshold")

	tup, err := buildValue(ctx, cns, largeJSON)
	require.NoError(t, err)

	// Confirm the tuple builder actually spilled out-of-band: a fresh
	// GetJsonAdaptiveValue read should return a JsonAdaptiveStorage
	// rather than a []byte.
	result, ok, err := valDesc.GetJsonAdaptiveValue(ctx, 0, cns, tup)
	require.NoError(t, err)
	require.True(t, ok)
	_, isOutOfBand := result.(*val.JsonAdaptiveStorage)
	require.Truef(t, isOutOfBand, "test setup: expected out-of-band storage, got %T", result)

	cns.Reset()

	newBytes, err := applyFieldMutations(ctx, cns, tup, []backends.FieldMutation{
		{Key: "email", Value: "new@example.com"},
	})
	require.NoError(t, err)

	require.Greaterf(t, cns.Writes(), int64(0),
		"out-of-band mutation must touch chunk store (IndexedJsonDocument path); got %d writes",
		cns.Writes())
	require.Contains(t, string(newBytes), `"new@example.com"`)
}
