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
	"fmt"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

// IndexedBsonDocument is a BSON document stored as a prolly tree of
// blob chunks, indexed by an AddressMap keyed by document path
// locations. Leaves contain raw BSON byte substrings (with container
// length prefixes intact); concatenating leaves in AddressMap key order
// reproduces the original document.
//
// This is the bson-a structural-sharing storage shape. Mutations that
// change a container's body length must patch each ancestor
// container's 4-byte little-endian length prefix; see Set / Insert /
// Remove (forthcoming commits) for the splice algorithm.
type IndexedBsonDocument struct {
	am prolly.AddressMap
	ns tree.NodeStore
}

// NewIndexedBsonDocument constructs an IndexedBsonDocument over an
// existing prolly AddressMap and node store. Used when the tree has
// already been built; the AddressMap is typically rehydrated from a
// persisted root via Open.
func NewIndexedBsonDocument(am prolly.AddressMap, ns tree.NodeStore) IndexedBsonDocument {
	return IndexedBsonDocument{am: am, ns: ns}
}

// Open rebuilds an IndexedBsonDocument from the root node of its
// AddressMap. The root node is the same one returned by AddressMap()'s
// HashOf(); callers persist that hash externally and rehydrate via
// Open on read.
func Open(ctx context.Context, ns tree.NodeStore, rootHash hash.Hash) (IndexedBsonDocument, error) {
	node, err := ns.Read(ctx, rootHash)
	if err != nil {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: open: %w", err)
	}
	am, err := prolly.NewAddressMap(node, ns)
	if err != nil {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: address map from node: %w", err)
	}
	return IndexedBsonDocument{am: am, ns: ns}, nil
}

// Root returns the hash of the AddressMap's root node. Persist this
// externally so the index can be reopened via Open.
func (d IndexedBsonDocument) Root() hash.Hash {
	return d.am.HashOf()
}

// AddressMap exposes the underlying prolly.AddressMap for callers that
// need to walk it directly (e.g. for diff or merge).
func (d IndexedBsonDocument) AddressMap() prolly.AddressMap {
	return d.am
}

// NodeStore returns the NodeStore the document is backed by.
func (d IndexedBsonDocument) NodeStore() tree.NodeStore {
	return d.ns
}

// Bytes materialises the full BSON byte sequence by walking the
// AddressMap in key order and concatenating leaf blobs. For bson-a
// the leaves already contain raw BSON (length prefixes intact) so
// concatenation alone yields wire-ready bytes.
func (d IndexedBsonDocument) Bytes(ctx context.Context) ([]byte, error) {
	var out []byte
	err := d.am.IterAll(ctx, func(_ string, addr hash.Hash) error {
		chunk, err := d.ns.ReadBytes(ctx, addr)
		if err != nil {
			return fmt.Errorf("bsonindexed: read leaf %s: %w", addr.String(), err)
		}
		out = append(out, chunk...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Count returns the number of leaf chunks in the AddressMap. Useful
// for tests and for the bake-off's leaves-rewritten instrumentation.
func (d IndexedBsonDocument) Count(ctx context.Context) (int, error) {
	return d.am.Count()
}
