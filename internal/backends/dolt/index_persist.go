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
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
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

// persistIndexes rebuilds state.collIndexAMs[collName] from the current
// in-memory state.indexes[collName] / state.secIndexMaps[collName].
//
// For each registered index it:
//  1. Writes the prolly.Map root node so cs.Get(mapRoot) works on reopen.
//  2. Writes a JSON IndexEntry chunk pairing IndexInfo metadata with the map root.
//  3. Adds (index name → IndexEntry hash) to a fresh AddressMap.
//
// The resulting AddressMap is cached so the next dtblHashForCollection call
// inlines the up-to-date secondary_indexes bytes into the DTBL.
//
// The caller must hold state.mu (write lock).
func (state *dbState) persistIndexes(ctx context.Context, collName string) error {
	idxInfos := state.indexes[collName]
	secMaps := state.secIndexMaps[collName]

	if len(idxInfos) == 0 {
		delete(state.collIndexAMs, collName)
		return nil
	}

	am, err := prolly.NewEmptyAddressMap(state.ns)
	if err != nil {
		return fmt.Errorf("dolt: creating index AM for %q: %w", collName, err)
	}
	edt := am.Editor()

	for _, idx := range idxInfos {
		m, ok := secMaps[idx.Name]
		if !ok {
			return fmt.Errorf("dolt: index %q on %q has no in-memory map", idx.Name, collName)
		}

		ref, err := state.vs.WriteValue(ctx, tree.ValueFromNode(m.Node()))
		if err != nil {
			return fmt.Errorf("dolt: writing index map root for %q.%q: %w", collName, idx.Name, err)
		}
		mapRoot := ref.TargetHash()

		entry, err := indexInfoToEntry(idx, mapRoot)
		if err != nil {
			return fmt.Errorf("dolt: encoding index entry for %q.%q: %w", collName, idx.Name, err)
		}
		entryHash, err := writeIndexEntryChunk(ctx, state.ns, entry)
		if err != nil {
			return fmt.Errorf("dolt: writing index entry chunk for %q.%q: %w", collName, idx.Name, err)
		}

		if err := edt.Add(ctx, idx.Name, entryHash); err != nil {
			return fmt.Errorf("dolt: adding %q to index AM: %w", idx.Name, err)
		}
	}

	newAM, err := edt.Flush(ctx)
	if err != nil {
		return fmt.Errorf("dolt: flushing index AM for %q: %w", collName, err)
	}
	state.collIndexAMs[collName] = newAM
	return nil
}

// loadIndexesFromDTBL parses the SecondaryIndexes AM from the DTBL chunk for
// collName and hydrates state.indexes[collName], state.secIndexMaps[collName]
// and state.collIndexAMs[collName].
//
// Called once per collection at db open. A legacy TUPM-format collection
// (no DTBL wrapper) is treated as having no secondary indexes.
//
// The caller must hold state.mu (write lock).
func (state *dbState) loadIndexesFromDTBL(ctx context.Context, collName string, dtblHash hash.Hash) error {
	if dtblHash.IsEmpty() {
		return nil
	}

	chunk, err := state.cs.Get(ctx, dtblHash)
	if err != nil {
		return fmt.Errorf("reading DTBL for %q: %w", collName, err)
	}
	if len(chunk.Data()) == 0 {
		return nil
	}
	if serial.GetFileID(chunk.Data()) != serial.TableFileID {
		// Legacy TUPM: no secondary indexes encoded at all.
		return nil
	}

	tbl, err := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
	if err != nil {
		return fmt.Errorf("parsing DTBL for %q: %w", collName, err)
	}
	idxBytes := tbl.SecondaryIndexesBytes()
	if len(idxBytes) == 0 {
		return nil
	}

	amNode, _, err := tree.NodeFromBytes(idxBytes)
	if err != nil {
		return fmt.Errorf("parsing index AM node for %q: %w", collName, err)
	}
	am, err := prolly.NewAddressMap(amNode, state.ns)
	if err != nil {
		return fmt.Errorf("loading index AM for %q: %w", collName, err)
	}

	cnt, err := am.Count()
	if err != nil {
		return fmt.Errorf("counting index AM for %q: %w", collName, err)
	}
	if cnt == 0 {
		return nil
	}

	infos := make([]backends.IndexInfo, 0, cnt)
	maps := make(map[string]prolly.Map, cnt)

	if err := am.IterAll(ctx, func(idxName string, entryHash hash.Hash) error {
		if entryHash.IsEmpty() {
			return nil
		}
		entry, err := readIndexEntryChunk(ctx, state.ns, entryHash)
		if err != nil {
			return fmt.Errorf("reading index entry for %q.%q: %w", collName, idxName, err)
		}
		idxInfo, mapRoot, err := entryToIndexInfo(entry)
		if err != nil {
			return fmt.Errorf("decoding index entry for %q.%q: %w", collName, idxName, err)
		}

		v, err := state.vs.ReadValue(ctx, mapRoot)
		if err != nil {
			return fmt.Errorf("reading secondary index map for %q.%q: %w", collName, idxName, err)
		}
		msg, ok := v.(dolttypes.SerialMessage)
		if !ok {
			return fmt.Errorf("unexpected value type %T for secondary index map %q.%q", v, collName, idxName)
		}
		node, _, err := tree.NodeFromBytes([]byte(msg))
		if err != nil {
			return fmt.Errorf("parsing secondary index map node for %q.%q: %w", collName, idxName, err)
		}
		secMap := prolly.NewMap(node, state.ns, idxpkg.KeyDescriptor(), idxpkg.ValDescriptor())

		infos = append(infos, idxInfo)
		maps[idxName] = secMap
		return nil
	}); err != nil {
		return err
	}

	slices.SortFunc(infos, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	state.collIndexAMs[collName] = am
	state.indexes[collName] = infos
	state.secIndexMaps[collName] = maps
	return nil
}

// hydrateAllIndexes iterates the collections AM and loads any persisted
// secondary indexes into in-memory state. Called once at db open.
//
// The caller must hold state.mu (write lock).
func (state *dbState) hydrateAllIndexes(ctx context.Context) error {
	return state.am.IterAll(ctx, func(collName string, dtblHash hash.Hash) error {
		return state.loadIndexesFromDTBL(ctx, collName, dtblHash)
	})
}
