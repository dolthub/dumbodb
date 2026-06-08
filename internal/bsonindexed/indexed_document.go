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

// IndexedBsonDocument is a BSON document stored as a prolly tree of blob
// chunks indexed by an AddressMap keyed by Location. Leaves contain raw
// BSON byte substrings (length prefixes intact); concatenating leaves in
// key order reproduces the original document.
type IndexedBsonDocument struct {
	am prolly.AddressMap
	ns tree.NodeStore
}

func NewIndexedBsonDocument(am prolly.AddressMap, ns tree.NodeStore) IndexedBsonDocument {
	return IndexedBsonDocument{am: am, ns: ns}
}

// Open rehydrates a document from the persisted root hash returned by
// Root().
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

// Root is the hash to persist externally; pass it back to Open to
// rehydrate.
func (d IndexedBsonDocument) Root() hash.Hash {
	return d.am.HashOf()
}

func (d IndexedBsonDocument) AddressMap() prolly.AddressMap {
	return d.am
}

func (d IndexedBsonDocument) NodeStore() tree.NodeStore {
	return d.ns
}

// Bytes concatenates leaves in AddressMap key order to yield wire-ready
// BSON.
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

func (d IndexedBsonDocument) Count(ctx context.Context) (int, error) {
	return d.am.Count()
}
