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

// Secondary-index resolution from the session's working root.
//
// See docs/design/branch-scoped-index-metadata.md sections 3.3 and 6.4.
//
// All functions in this file are pure with respect to dbState: they read
// from the chunk store, NodeStore, and ValueStore handed to them, but
// touch no in-memory caches keyed by collection name. The single
// process-wide memo here (indexEntryMemo) is content-addressed on the
// IndexEntry chunk hash, so it is safe to share across branches and
// databases.

import (
	"context"
	"fmt"
	"sync"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
)

// resolvedIndexEntry is the decoded form of an IndexEntry chunk: the
// metadata plus the hash of the index's prolly.Map root.
type resolvedIndexEntry struct {
	info    backends.IndexInfo
	mapRoot hash.Hash
}

// indexEntryMemo caches resolveIndexEntry results. Hashes are immutable
// and content-addressed; the same hash from any branch or database
// resolves to the same metadata. No eviction; see design 6.1.
var indexEntryMemo sync.Map // hash.Hash -> *resolvedIndexEntry

// emptyIndexAMCache memoizes the per-NodeStore empty AddressMap so writes
// that have no secondary indexes do not re-allocate a fresh empty AM.
// Replaces the per-dbState state.emptyIndexAM field; see design 6.4.
var emptyIndexAMCache sync.Map // tree.NodeStore -> prolly.AddressMap

// emptyIndexAM returns the cached empty AddressMap for ns, building it on
// first call.
func emptyIndexAM(ns tree.NodeStore) (prolly.AddressMap, error) {
	if v, ok := emptyIndexAMCache.Load(ns); ok {
		return v.(prolly.AddressMap), nil
	}
	am, err := prolly.NewEmptyAddressMap(ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("emptyIndexAM: %w", err)
	}
	actual, _ := emptyIndexAMCache.LoadOrStore(ns, am)
	return actual.(prolly.AddressMap), nil
}

// dtblHashForColl returns the DTBL hash for collName under rv. Returns
// the zero hash if the collection is not in the root's ADRM.
func dtblHashForColl(ctx context.Context, ns tree.NodeStore, rv doltdb.RootValue, collName string) (hash.Hash, error) {
	if rv == nil {
		return hash.Hash{}, nil
	}
	am, err := amFromWorkingRoot(ctx, rv, ns)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dtblHashForColl: %w", err)
	}
	h, err := am.Get(ctx, collName)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dtblHashForColl: %s: %w", collName, err)
	}
	return h, nil
}

// indexAMForDTBL returns the secondary_indexes AddressMap inlined in the
// DTBL chunk at dtblHash. Returns the shared empty AM if dtblHash is the
// zero hash, the chunk is empty, the chunk is a legacy TUPM (no secondary
// indexes encoded), or the DTBL inlines an empty secondary_indexes field.
func indexAMForDTBL(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, dtblHash hash.Hash) (prolly.AddressMap, error) {
	if dtblHash.IsEmpty() {
		return emptyIndexAM(ns)
	}
	chunk, err := cs.Get(ctx, dtblHash)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: reading DTBL chunk: %w", err)
	}
	data := chunk.Data()
	if len(data) == 0 {
		return emptyIndexAM(ns)
	}
	if serial.GetFileID(data) != serial.TableFileID {
		// Legacy TUPM has no secondary indexes encoded.
		return emptyIndexAM(ns)
	}
	tbl, err := serial.TryGetRootAsTable(data, serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: parsing DTBL: %w", err)
	}
	idxBytes := tbl.SecondaryIndexesBytes()
	if len(idxBytes) == 0 {
		return emptyIndexAM(ns)
	}
	amNode, _, err := tree.NodeFromBytes(idxBytes)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: parsing AM node: %w", err)
	}
	am, err := prolly.NewAddressMap(amNode, ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: building AM: %w", err)
	}
	return am, nil
}

// resolveIndexEntry decodes the IndexEntry JSON chunk at entryHash via
// the process-wide memo. The returned pointer must not be mutated by the
// caller. See design 6.5 for why memoizing is safe (chunk bytes are
// immutable; the captured MatchesPartialFilter closure is pure).
func resolveIndexEntry(ctx context.Context, ns tree.NodeStore, entryHash hash.Hash) (*resolvedIndexEntry, error) {
	if entryHash.IsEmpty() {
		return nil, fmt.Errorf("resolveIndexEntry: empty hash")
	}
	if v, ok := indexEntryMemo.Load(entryHash); ok {
		return v.(*resolvedIndexEntry), nil
	}
	entry, err := readIndexEntryChunk(ctx, ns, entryHash)
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: %w", err)
	}
	info, mapRoot, err := entryToIndexInfo(entry)
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: decode: %w", err)
	}
	resolved := &resolvedIndexEntry{info: info, mapRoot: mapRoot}
	actual, _ := indexEntryMemo.LoadOrStore(entryHash, resolved)
	return actual.(*resolvedIndexEntry), nil
}

// openIndexMap returns a prolly.Map handle for the secondary-index data
// stored at mapRoot.
func openIndexMap(ctx context.Context, vs *dolttypes.ValueStore, ns tree.NodeStore, mapRoot hash.Hash) (prolly.Map, error) {
	v, err := vs.ReadValue(ctx, mapRoot)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("openIndexMap: ReadValue: %w", err)
	}
	msg, ok := v.(dolttypes.SerialMessage)
	if !ok {
		return prolly.Map{}, fmt.Errorf("openIndexMap: unexpected value type %T", v)
	}
	node, _, err := tree.NodeFromBytes([]byte(msg))
	if err != nil {
		return prolly.Map{}, fmt.Errorf("openIndexMap: NodeFromBytes: %w", err)
	}
	return prolly.NewMap(node, ns, idxpkg.KeyDescriptor(), idxpkg.ValDescriptor()), nil
}
