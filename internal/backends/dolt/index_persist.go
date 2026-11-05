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
	"errors"
	"fmt"
	"io"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// secondaryIndexes is a collection's DTBL secondary_indexes field: a prolly
// map from index name to the index's definition and the root of its index
// map. The root is an address field, so chunk walks (GC, push, clone, backup)
// reach the index map.
type secondaryIndexes struct {
	ns tree.NodeStore
	m  prolly.Map
}

var secondaryIndexKeyDesc = val.NewTupleDescriptor(val.Type{Enc: val.StringEnc, Nullable: false})

// The root is stored as BytesAddrEnc, the encoding chunk walks follow; it
// addresses an index map, not a blob.
func secondaryIndexValDescFor(ns tree.NodeStore) *val.TupleDesc {
	return val.NewTupleDescriptorWithArgs(
		val.TupleDescriptorArgs{ValueStore: ns},
		val.Type{Enc: val.BytesAdaptiveEnc, Nullable: false},
		val.Type{Enc: val.BytesAddrEnc, Nullable: false},
	)
}

// secondaryIndexValReadDesc only locates fields; see storedValueReadDesc.
var secondaryIndexValReadDesc = val.NewTupleDescriptor(
	val.Type{Enc: val.BytesAdaptiveEnc, Nullable: false},
	val.Type{Enc: val.BytesAddrEnc, Nullable: false},
)

func newSecondaryIndexes(ctx context.Context, ns tree.NodeStore) (secondaryIndexes, error) {
	m, err := prolly.NewMapFromTuples(ctx, ns, secondaryIndexKeyDesc, secondaryIndexValDescFor(ns))
	if err != nil {
		return secondaryIndexes{}, err
	}
	return secondaryIndexes{ns: ns, m: m}, nil
}

// errUnsupportedIndexLayout marks tables written before dolthub/dumbodb#190,
// which hold an AddressMap in secondary_indexes.
var errUnsupportedIndexLayout = errors.New("a secondary index layout this version does not read")

func secondaryIndexesFromBytes(ns tree.NodeStore, b []byte) (secondaryIndexes, error) {
	if id := serial.GetFileID(b); id != serial.ProllyTreeNodeFileID {
		return secondaryIndexes{}, fmt.Errorf("secondary indexes stored as %q, %w; recreate the database", id, errUnsupportedIndexLayout)
	}
	node, _, err := tree.NodeFromBytes(b)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("parsing secondary indexes node: %w", err)
	}
	return secondaryIndexes{ns: ns, m: prolly.NewMap(node, ns, secondaryIndexKeyDesc, secondaryIndexValDescFor(ns))}, nil
}

func (s secondaryIndexes) Count() (int, error) {
	return s.m.Count()
}

// IterAll calls cb with each index in name order.
func (s secondaryIndexes) IterAll(ctx context.Context, cb func(name string, entry *resolvedIndexEntry) error) error {
	iter, err := s.m.IterAll(ctx)
	if err != nil {
		return err
	}
	for {
		k, v, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, _ := secondaryIndexKeyDesc.GetString(0, k)
		def, root, err := readSecondaryIndexValue(ctx, s.ns, v)
		if err != nil {
			return fmt.Errorf("reading secondary index %q: %w", name, err)
		}
		entry, err := resolveIndexEntry(def, root)
		if err != nil {
			return fmt.Errorf("decoding secondary index %q: %w", name, err)
		}
		if err := cb(name, entry); err != nil {
			return err
		}
	}
}

// tableBytes returns the secondary_indexes field for a DTBL.
func (s secondaryIndexes) tableBytes() []byte {
	return []byte(tree.ValueFromNode(s.m.Node()).(dolttypes.SerialMessage))
}

// secondaryIndexesFromRoots builds the secondary indexes of infos, whose index
// maps are written at roots.
func secondaryIndexesFromRoots(ctx context.Context, ns tree.NodeStore, infos []backends.IndexInfo, roots []hash.Hash) (secondaryIndexes, error) {
	s, err := newSecondaryIndexes(ctx, ns)
	if err != nil {
		return secondaryIndexes{}, err
	}
	mut := s.m.Mutate()
	valDesc := secondaryIndexValDescFor(ns)
	for i, idx := range infos {
		def, err := encodeIndexDefinition(idx)
		if err != nil {
			return secondaryIndexes{}, fmt.Errorf("encoding definition of %q: %w", idx.Name, err)
		}
		kb := val.NewTupleBuilder(secondaryIndexKeyDesc, nil)
		if err := kb.PutString(0, idx.Name); err != nil {
			return secondaryIndexes{}, err
		}
		k, err := kb.Build(ctx, bufPool)
		if err != nil {
			return secondaryIndexes{}, err
		}
		vb := val.NewTupleBuilder(valDesc, ns)
		if err := vb.PutAdaptiveBytesFromInline(ctx, 0, def); err != nil {
			return secondaryIndexes{}, err
		}
		vb.PutBytesAddr(1, roots[i])
		v, err := vb.Build(ctx, bufPool)
		if err != nil {
			return secondaryIndexes{}, err
		}
		if err := mut.Put(ctx, k, v); err != nil {
			return secondaryIndexes{}, err
		}
	}
	if s.m, err = mut.Map(ctx); err != nil {
		return secondaryIndexes{}, err
	}
	return s, nil
}

func readSecondaryIndexValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) (def []byte, root hash.Hash, err error) {
	if rootLen := len(secondaryIndexValReadDesc.GetField(1, v)); rootLen != hash.ByteLen {
		return nil, hash.Hash{}, fmt.Errorf("invalid index map root length %d", rootLen)
	}
	root, _ = secondaryIndexValReadDesc.GetBytesAddr(1, v)
	stored, ok, err := secondaryIndexValReadDesc.GetBytesAdaptiveValue(ctx, 0, ns, v)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	if !ok {
		return nil, hash.Hash{}, fmt.Errorf("value missing definition")
	}
	switch stored := stored.(type) {
	case []byte:
		return stored, root, nil
	case *val.ByteArray:
		def, err := appendBlobBytes(ctx, ns, stored.Addr, nil)
		return def, root, err
	default:
		return nil, hash.Hash{}, fmt.Errorf("unexpected definition value %T", stored)
	}
}

// encodeIndexDefinition encodes idx as BSON in canonical form, keys sorted at
// every level, so a definition has the same bytes however it was written.
func encodeIndexDefinition(idx backends.IndexInfo) ([]byte, error) {
	keys := types.MakeArray(len(idx.Key))
	for _, k := range idx.Key {
		kd, err := types.NewDocument(
			"field", k.Field,
			"desc", k.Descending,
			"text", k.Text,
			"geo2d", k.Geo2D,
			"geo2dsphere", k.Geo2DSphere,
			"hashed", k.Hashed,
		)
		if err != nil {
			return nil, err
		}
		keys.Append(kd)
	}
	doc, err := types.NewDocument(
		"name", idx.Name,
		"keys", keys,
		"unique", idx.Unique,
		"sparse", idx.Sparse,
		"hidden", idx.Hidden,
		"lossy", idx.Lossy,
		"multikey", idx.Multikey,
	)
	if err != nil {
		return nil, err
	}
	if idx.PartialFilterExpression != nil {
		doc.Set("partial", idx.PartialFilterExpression)
	}
	if idx.Collation != nil {
		doc.Set("collation", idx.Collation)
	}
	return doc.AppendBSONSortedKeys(nil), nil
}

// decodeIndexDefinition decodes an encodeIndexDefinition document.
// MatchesPartialFilter calls backends.MatchPartialFilter, whose predicate the
// handler layer registers at init, keeping the backend free of a dependency
// on handler/common.
func decodeIndexDefinition(doc *types.Document) (backends.IndexInfo, error) {
	getBool := func(d *types.Document, key string) bool {
		v, _ := d.Get(key)
		b, _ := v.(bool)
		return b
	}
	getDoc := func(key string) *types.Document {
		v, _ := doc.Get(key)
		d, _ := v.(*types.Document)
		return d
	}
	nameVal, _ := doc.Get("name")
	name, _ := nameVal.(string)
	info := backends.IndexInfo{
		Name:     name,
		Unique:   getBool(doc, "unique"),
		Sparse:   getBool(doc, "sparse"),
		Hidden:   getBool(doc, "hidden"),
		Lossy:    getBool(doc, "lossy"),
		Multikey: getBool(doc, "multikey"),
	}
	keysVal, err := doc.Get("keys")
	if err != nil {
		return backends.IndexInfo{}, fmt.Errorf("index definition missing keys")
	}
	arr, ok := keysVal.(*types.Array)
	if !ok {
		return backends.IndexInfo{}, fmt.Errorf("index definition keys is %T, want array", keysVal)
	}
	for i := 0; i < arr.Len(); i++ {
		kv, _ := arr.Get(i)
		kd, ok := kv.(*types.Document)
		if !ok {
			return backends.IndexInfo{}, fmt.Errorf("index definition key %d is %T, want document", i, kv)
		}
		fieldVal, err := kd.Get("field")
		if err != nil {
			return backends.IndexInfo{}, fmt.Errorf("index definition key %d missing field", i)
		}
		field, _ := fieldVal.(string)
		info.Key = append(info.Key, backends.IndexKeyPair{
			Field:       field,
			Descending:  getBool(kd, "desc"),
			Text:        getBool(kd, "text"),
			Geo2D:       getBool(kd, "geo2d"),
			Geo2DSphere: getBool(kd, "geo2dsphere"),
			Hashed:      getBool(kd, "hashed"),
		})
	}
	info.PartialFilterExpression = getDoc("partial")
	info.Collation = getDoc("collation")
	if pf := info.PartialFilterExpression; pf != nil {
		info.MatchesPartialFilter = func(doc *types.Document) (bool, error) {
			return backends.MatchPartialFilter(doc, pf)
		}
	}
	return info, nil
}
