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
	"fmt"
	"io"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
)

// Serialize chunks raw BSON bytes into a prolly tree of blob leaves
// indexed by Location-keyed AddressMap. The returned IndexedBsonDocument
// references that tree; the chunk store contents are persisted via ns.
//
// Chunk boundaries are chosen by walking the BSON document with the
// Scanner and, at each named boundary, applying the weibull-shaped
// content-defined chunking decision used by dolt's prolly tree. Bytes
// between successive chunk boundaries form the body of a leaf blob;
// the AddressMap key is the LocationKey at the END of the chunk's
// span, mirroring dolt's JSON chunker convention.
//
// For a document smaller than MinChunkSize the result is a single leaf
// with the whole document; the AddressMap has one entry keyed by
// end-of-document.
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
		// We only consider creating a chunk boundary at the natural
		// post-element points: EndOfValue. ObjectInitialElement and
		// ArrayInitialElement boundaries are useful as path landmarks
		// but a chunk that begins immediately after entering a
		// container is rare and the cost of an extra split outweighs
		// the lookup-locality benefit. StartOfValue is observed as
		// each element opens; we don't chunk there.
		if s.Path().State() != EndOfValue {
			continue
		}
		key := s.Path().Key()
		span := s.Pos() - chunkStart
		if !CrossesBoundary(key, uint32(span)) {
			continue
		}
		// Materialise this chunk: bytes [chunkStart, s.Pos()) and the
		// LocationKey at the boundary.
		blobAddr, err := writeBlob(ctx, ns, bsonBytes[chunkStart:s.Pos()])
		if err != nil {
			return IndexedBsonDocument{}, err
		}
		if err := ed.Add(ctx, string(bytes.Clone(key)), blobAddr); err != nil {
			return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: addressmap add: %w", err)
		}
		chunkStart = s.Pos()
	}
	// Final chunk: everything from chunkStart to end of document
	// belongs to one trailing leaf. The end-of-document key is the
	// special 0xFF byte that sorts after every path-level key.
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

// writeBlob writes a leaf blob containing data and returns the
// resulting node address. Uses NodeStore.WriteBytes which constructs a
// Blob serial message under the hood.
func writeBlob(ctx context.Context, ns tree.NodeStore, data []byte) (hash.Hash, error) {
	addr, err := ns.WriteBytes(ctx, bytes.Clone(data))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("bsonindexed: blob write: %w", err)
	}
	return addr, nil
}
