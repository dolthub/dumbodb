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

// seekCounter tallies prolly-tree node fetches for one operation, scoped to a
// context by WithSeekCounter.
type seekCounter struct {
	nodes atomic.Int64
	calls atomic.Int64
}

func (c *seekCounter) Nodes() int64 { return c.nodes.Load() }
func (c *seekCounter) Calls() int64 { return c.calls.Load() }

type seekCounterKeyType struct{}

var seekCounterKey seekCounterKeyType

// WithSeekCounter returns a child context carrying a fresh counter, and the
// counter; node fetches through that context accrue to it.
func WithSeekCounter(ctx context.Context) (context.Context, *seekCounter) {
	c := &seekCounter{}
	return context.WithValue(ctx, seekCounterKey, c), c
}

func seekCounterFrom(ctx context.Context) *seekCounter {
	c, _ := ctx.Value(seekCounterKey).(*seekCounter)
	return c
}

// instrumentedNodeStore counts node fetches against a context-scoped seekCounter
// when one is present, and is otherwise a transparent pass-through.
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
