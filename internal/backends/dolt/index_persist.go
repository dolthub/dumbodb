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
	"fmt"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

// indexEntryDoc is the on-disk shape for a single secondary index
// entry (stored as a BSON chunk): the IndexInfo metadata plus the
// 20-byte hash of the index's prolly.Map root.
type indexEntryDoc struct {
	Name             string         `json:"name"`
	Keys             []indexKeyJSON `json:"keys"`
	Unique           bool           `json:"unique,omitempty"`
	Sparse           bool           `json:"sparse,omitempty"`
	PartialBSONHex   string         `json:"partial,omitempty"`   // hex-encoded BSON of PartialFilterExpression
	CollationBSONHex string         `json:"collation,omitempty"` // hex-encoded BSON of the collation spec
	Lossy            bool           `json:"lossy,omitempty"`     // index stored a value with no faithful encoding
	Multikey         bool           `json:"multikey,omitempty"`  // index expanded an array value into per-element entries
	MapRoot          string         `json:"map_root"`            // hex-encoded 20-byte hash
}

type indexKeyJSON struct {
	Field       string `json:"field"`
	Descending  bool   `json:"desc,omitempty"`
	Text        bool   `json:"text,omitempty"`
	Geo2D       bool   `json:"geo2d,omitempty"`
	Geo2DSphere bool   `json:"geo2dsphere,omitempty"`
	Hashed      bool   `json:"hashed,omitempty"`
}

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
	pfHex, err := docToHex(idx.PartialFilterExpression)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("encoding partial filter: %w", err)
	}
	collHex, err := docToHex(idx.Collation)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("encoding collation: %w", err)
	}
	return indexEntryDoc{
		Name:             idx.Name,
		Keys:             keys,
		Unique:           idx.Unique,
		Sparse:           idx.Sparse,
		PartialBSONHex:   pfHex,
		CollationBSONHex: collHex,
		Lossy:            idx.Lossy,
		Multikey:         idx.Multikey,
		MapRoot:          hex.EncodeToString(mapRoot[:]),
	}, nil
}

// docToHex encodes an optional document as hex-encoded BSON; a nil document
// yields "".
func docToHex(doc *types.Document) (string, error) {
	if doc == nil {
		return "", nil
	}
	wdoc, err := bson.FromDocument(doc)
	if err != nil {
		return "", err
	}
	b, err := wdoc.Encode()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hexToDoc decodes hex-encoded BSON produced by docToHex; "" yields nil.
func hexToDoc(h string) (*types.Document, error) {
	if h == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	return bson.ToDocument(wirebson.RawDocument(b))
}

// entryToIndexInfo rebuilds MatchesPartialFilter to call
// backends.MatchPartialFilter against the decoded BSON. The handler layer
// registers the underlying predicate (FilterDocument) at init time, which keeps
// the backend free of a circular dependency on handler/common.
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
	pf, err := hexToDoc(d.PartialBSONHex)
	if err != nil {
		return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("decoding partial filter: %w", err)
	}
	coll, err := hexToDoc(d.CollationBSONHex)
	if err != nil {
		return backends.IndexInfo{}, hash.Hash{}, fmt.Errorf("decoding collation: %w", err)
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
		Collation:               coll,
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

func writeIndexEntryChunk(ctx context.Context, ns tree.NodeStore, entry indexEntryDoc) (hash.Hash, error) {
	doc, err := indexEntryToDocument(entry)
	if err != nil {
		return hash.Hash{}, err
	}
	stored, err := docToBSON(doc)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("encoding index entry BSON: %w", err)
	}
	addr, err := ns.WriteBytes(ctx, stored)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing index entry chunk: %w", err)
	}
	return addr, nil
}

func readIndexEntryChunk(ctx context.Context, ns tree.NodeStore, h hash.Hash) (indexEntryDoc, error) {
	stored, err := ns.ReadBytes(ctx, h)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("reading index entry chunk: %w", err)
	}
	doc, err := bsonToDoc(stored)
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("decoding index entry BSON: %w", err)
	}
	return documentToIndexEntry(doc)
}

// indexEntryToDocument: field names match the historical JSON keys.
func indexEntryToDocument(entry indexEntryDoc) (*types.Document, error) {
	keys := types.MakeArray(len(entry.Keys))
	for _, k := range entry.Keys {
		kd, err := types.NewDocument(
			"field", k.Field,
			"desc", k.Descending,
			"text", k.Text,
			"geo2d", k.Geo2D,
			"geo2dsphere", k.Geo2DSphere,
			"hashed", k.Hashed,
		)
		if err != nil {
			return nil, fmt.Errorf("encoding index key pair: %w", err)
		}
		keys.Append(kd)
	}
	doc, err := types.NewDocument(
		"name", entry.Name,
		"keys", keys,
		"unique", entry.Unique,
		"sparse", entry.Sparse,
		"partial", entry.PartialBSONHex,
		"collation", entry.CollationBSONHex,
		"lossy", entry.Lossy,
		"multikey", entry.Multikey,
		"map_root", entry.MapRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("encoding index entry document: %w", err)
	}
	return doc, nil
}

func documentToIndexEntry(doc *types.Document) (indexEntryDoc, error) {
	var entry indexEntryDoc
	getString := func(key string) string {
		v, err := doc.Get(key)
		if err != nil {
			return ""
		}
		s, _ := v.(string)
		return s
	}
	getBool := func(d *types.Document, key string) bool {
		v, err := d.Get(key)
		if err != nil {
			return false
		}
		b, _ := v.(bool)
		return b
	}
	entry.Name = getString("name")
	entry.Unique = getBool(doc, "unique")
	entry.Sparse = getBool(doc, "sparse")
	entry.PartialBSONHex = getString("partial")
	entry.CollationBSONHex = getString("collation")
	entry.Lossy = getBool(doc, "lossy")
	entry.Multikey = getBool(doc, "multikey")
	entry.MapRoot = getString("map_root")

	keysVal, err := doc.Get("keys")
	if err != nil {
		return indexEntryDoc{}, fmt.Errorf("index entry document missing keys")
	}
	arr, ok := keysVal.(*types.Array)
	if !ok {
		return indexEntryDoc{}, fmt.Errorf("index entry keys is %T, want array", keysVal)
	}
	for i := 0; i < arr.Len(); i++ {
		kv, _ := arr.Get(i)
		kd, ok := kv.(*types.Document)
		if !ok {
			return indexEntryDoc{}, fmt.Errorf("index entry key %d is %T, want document", i, kv)
		}
		fieldVal, ferr := kd.Get("field")
		if ferr != nil {
			return indexEntryDoc{}, fmt.Errorf("index entry key %d missing field", i)
		}
		field, _ := fieldVal.(string)
		entry.Keys = append(entry.Keys, indexKeyJSON{
			Field:       field,
			Descending:  getBool(kd, "desc"),
			Text:        getBool(kd, "text"),
			Geo2D:       getBool(kd, "geo2d"),
			Geo2DSphere: getBool(kd, "geo2dsphere"),
			Hashed:      getBool(kd, "hashed"),
		})
	}
	return entry, nil
}
