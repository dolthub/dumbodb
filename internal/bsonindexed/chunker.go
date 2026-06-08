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
	"io"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

// Serialize chunks bsonBytes into a prolly tree of blob leaves indexed
// by a Location-keyed AddressMap. Boundaries are picked only at
// EndOfValue (content-defined via CrossesBoundary); a doc under
// MinChunkSize produces a single trailing leaf keyed by
// EndOfDocumentKey.
func Serialize(ctx context.Context, ns tree.NodeStore, bsonBytes []byte) (IndexedBsonDocument, error) {
	if len(bsonBytes) < 5 {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: BSON document must be at least 5 bytes, got %d", len(bsonBytes))
	}
	editor, err := prolly.NewEmptyAddressMap(ns)
	if err != nil {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: new address map: %w", err)
	}
	ed := editor.Editor()

	s := NewScanner(bsonBytes)
	chunkStart := 0

	for {
		err := s.AdvanceToNextLocation()
		if err == io.EOF {
			break
		}
		if err != nil {
			return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: scanner: %w", err)
		}
		if s.Path().State() != EndOfValue {
			continue
		}
		key := s.Path().Key()
		span := s.Pos() - chunkStart
		if !CrossesBoundary(key, uint32(span)) {
			continue
		}
		blobAddr, err := writeBlob(ctx, ns, bsonBytes[chunkStart:s.Pos()])
		if err != nil {
			return IndexedBsonDocument{}, err
		}
		if err := ed.Add(ctx, string(bytes.Clone(key)), blobAddr); err != nil {
			return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: addressmap add: %w", err)
		}
		chunkStart = s.Pos()
	}
	if chunkStart < len(bsonBytes) {
		blobAddr, err := writeBlob(ctx, ns, bsonBytes[chunkStart:])
		if err != nil {
			return IndexedBsonDocument{}, err
		}
		if err := ed.Add(ctx, string(EndOfDocumentKey), blobAddr); err != nil {
			return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: addressmap final add: %w", err)
		}
	}

	am, err := ed.Flush(ctx)
	if err != nil {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: addressmap flush: %w", err)
	}
	return IndexedBsonDocument{am: am, ns: ns}, nil
}

func writeBlob(ctx context.Context, ns tree.NodeStore, data []byte) (hash.Hash, error) {
	addr, err := ns.WriteBytes(ctx, bytes.Clone(data))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("bsonindexed: blob write: %w", err)
	}
	return addr, nil
}
