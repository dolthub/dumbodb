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
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

// indexEntryDoc is the on-disk JSON shape for a single secondary index entry.
// It pairs the IndexInfo metadata (so listIndexes survives restart) with the
// 20-byte hash of the index's prolly.Map root chunk (so the data survives too).
type indexEntryDoc struct {
	Name           string         `json:"name"`
	Keys           []indexKeyJSON `json:"keys"`
	Unique         bool           `json:"unique,omitempty"`
	Sparse         bool           `json:"sparse,omitempty"`
	PartialBSONHex string         `json:"partial,omitempty"` // hex-encoded BSON of PartialFilterExpression
	Lossy          bool           `json:"lossy,omitempty"`   // index stored a value with no faithful encoding
	Multikey       bool           `json:"multikey,omitempty"` // index expanded an array value into per-element entries
	MapRoot        string         `json:"map_root"`          // hex-encoded 20-byte hash
}

// indexKeyJSON mirrors backends.IndexKeyPair for JSON encoding.
type indexKeyJSON struct {
	Field       string `json:"field"`
	Descending  bool   `json:"desc,omitempty"`
	Text        bool   `json:"text,omitempty"`
	Geo2D       bool   `json:"geo2d,omitempty"`
	Geo2DSphere bool   `json:"geo2dsphere,omitempty"`
	Hashed      bool   `json:"hashed,omitempty"`
}

// indexInfoToEntry converts a backends.IndexInfo plus the hash of the index's
// prolly.Map root into the JSON shape stored in the chunk store.
func indexInfoToEntry(idx backends.IndexInfo, mapRoot hash.Hash) (indexEntryDoc, error) {
	keys := make([]indexKeyJSON, len(idx.Key))
	for i, k := range idx.Key {
		keys[i] = indexKeyJSON{
			Field:       k.Field,
			Descending:  k.Descending,
			Text:        k.Text,
			Geo2D:       k.Geo2D,
			Geo2DSphere: k.Geo2DSphere,
			Hashed:      k.Hashed,
		}
	}
	var pfHex string
	if idx.PartialFilterExpression != nil {
		wdoc, err := bson.FromDocument(idx.PartialFilterExpression)
		if err != nil {
			return indexEntryDoc{}, fmt.Errorf("encoding partial filter to wirebson: %w", err)
		}
		pfBytes, err := wdoc.Encode()
		if err != nil {
			return indexEntryDoc{}, fmt.Errorf("encoding partial filter BSON: %w", err)
		}
		pfHex = hex.EncodeToString(pfBytes)
	}
	return indexEntryDoc{
		Name:           idx.Name,
		Keys:           keys,
		Unique:         idx.Unique,
		Sparse:         idx.Sparse,
		PartialBSONHex: pfHex,
		Lossy:          idx.Lossy,
		Multikey:       idx.Multikey,
		MapRoot:        hex.EncodeToString(mapRoot[:]),
	}, nil
}

// entryToIndexInfo decodes a stored entry back into an IndexInfo and the
// 20-byte hash of the secondary-index prolly.Map root.
//
// When the entry has a persisted partial filter expression, MatchesPartialFilter
// is rebuilt to call backends.MatchPartialFilter against the decoded BSON. The
// handler layer registers the underlying predicate (FilterDocument) at init time,
// which keeps the backend free of a circular dependency on handler/common.
func entryToIndexInfo(d indexEntryDoc) (backends.IndexInfo, hash.Hash, error) {
	keys := make([]backends.IndexKeyPair, len(d.Keys))
	for i, k := range d.Keys {
		keys[i] = backends.IndexKeyPair{
			Field:       k.Field,
			Descending:  k.Descending,
			Text:        k.Text,
			Geo2D:       k.Geo2D,
			Geo2DSphere: k.Geo2DSphere,
			Hashed:      k.Hashed,
		}
	}
	var pf *types.Document
	if d.PartialBSONHex != "" {
		pfBytes, err := hex.DecodeString(d.PartialBSONHex)
		if err != nil {
			return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("decoding partial filter hex: %w", err)
		}
		pf, err = bson.ToDocument(wirebson.RawDocument(pfBytes))
		if err != nil {
			return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("decoding partial filter BSON: %w", err)
		}
	}
	rootBytes, err := hex.DecodeString(d.MapRoot)
	if err != nil {
		return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("decoding map root hex: %w", err)
	}
	if len(rootBytes) != hash.ByteLen {
		return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("invalid map root length %d (want %d)", len(rootBytes), hash.ByteLen)
	}
	var root hash.Hash
	copy(root[:], rootBytes)
	info := backends.IndexInfo{
		Name:                    d.Name,
		Key:                     keys,
		Unique:                  d.Unique,
		Sparse:                  d.Sparse,
		PartialFilterExpression: pf,
		Lossy:                   d.Lossy,
		Multikey:                d.Multikey,
	}
	if pf != nil {
		captured := pf
		info.MatchesPartialFilter = func(doc *types.Document) (bool, error) {
			return backends.MatchPartialFilter(doc, captured)
		}
	}
	return info, root, nil
}

// writeIndexEntryChunk serialises an index entry as JSON, stores it in the
// dolt chunk store via the JSON tree path, and returns the root hash.
func writeIndexEntryChunk(ctx context.Context, ns tree.NodeStore, entry indexEntryDoc) (hash.Hash, error) {
	jsonBytes, err := json.Marshal(entry)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("encoding index entry JSON: %w", err)
	}
	wrapper := sqltypes.NewLazyJSONDocument(jsonBytes)
	root, err := tree.SerializeJsonToAddr(ctx, ns, wrapper)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("serialising index entry JSON: %w", err)
	}
	return root.HashOf(), nil
}

// readIndexEntryChunk reads the JSON-encoded index entry stored at h and
// decodes it.
func readIndexEntryChunk(ctx context.Context, ns tree.NodeStore, h hash.Hash) (indexEntryDoc, error) {
	jsonDoc := tree.NewJSONDoc(h, ns)
	wrapper, err := jsonDoc.ToIndexedJSONDocument(ctx)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("loading index entry JSON: %w", err)
	}
	jsonBytes, err := sqltypes.MarshallJson(ctx, wrapper)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("marshalling index entry JSON: %w", err)
	}
	var entry indexEntryDoc
	if err := json.Unmarshal(jsonBytes, &entry); err != nil {
		return indexEntryDoc{}, fmt.Errorf("decoding index entry JSON: %w", err)
	}
	return entry, nil
}

