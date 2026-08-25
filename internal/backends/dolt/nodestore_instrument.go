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
	"sync/atomic"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

// seekCounter tallies prolly-tree node fetches for a single operation. It is the
// deterministic, timing-free measure of seek cost: a point lookup descends
// root->leaf, so nodes read grows with tree height (~log N), not collection
// size (N). A caller scopes one via WithSeekCounter and reads the tally after
// the operation returns.
type seekCounter struct {
	nodes atomic.Int64 // tree nodes fetched (Read: +1, ReadMany: +len(refs))
	calls atomic.Int64 // Read/ReadMany invocations
}

// Nodes returns the number of prolly-tree nodes fetched since the counter was
// created.
func (c *seekCounter) Nodes() int64 { return c.nodes.Load() }

// Calls returns the number of NodeStore Read/ReadMany invocations.
func (c *seekCounter) Calls() int64 { return c.calls.Load() }

type seekCounterKeyType struct{}

var seekCounterKey seekCounterKeyType

// WithSeekCounter returns a child context carrying a fresh seekCounter and the
// counter itself. Node fetches by any instrumentedNodeStore reached through the
// returned context accrue to the counter; a context without one leaves every
// instrumented store on its zero-cost path.
func WithSeekCounter(ctx context.Context) (context.Context, *seekCounter) {
	c := &seekCounter{}
	return context.WithValue(ctx, seekCounterKey, c), c
}

func seekCounterFrom(ctx context.Context) *seekCounter {
	c, _ := ctx.Value(seekCounterKey).(*seekCounter)
	return c
}

// instrumentedNodeStore wraps a tree.NodeStore to count node fetches per
// context-scoped operation. Embedding promotes every other NodeStore method
// unchanged, so the wrapper stays correct if the interface grows. Counting is
// gated on a context-carried seekCounter: with none present the overhead is a
// single nil-returning context lookup, so it is safe to install unconditionally
// on the production read path.
type instrumentedNodeStore struct {
	tree.NodeStore
}

func newInstrumentedNodeStore(ns tree.NodeStore) tree.NodeStore {
	return instrumentedNodeStore{NodeStore: ns}
}

func (s instrumentedNodeStore) Read(ctx context.Context, ref hash.Hash) (*tree.Node, error) {
	if c := seekCounterFrom(ctx); c != nil {
		c.nodes.Add(1)
		c.calls.Add(1)
	}
	return s.NodeStore.Read(ctx, ref)
}

func (s instrumentedNodeStore) ReadMany(ctx context.Context, refs hash.HashSlice) ([]*tree.Node, error) {
	if c := seekCounterFrom(ctx); c != nil {
		c.nodes.Add(int64(len(refs)))
		c.calls.Add(1)
	}
	return s.NodeStore.ReadMany(ctx, refs)
}
