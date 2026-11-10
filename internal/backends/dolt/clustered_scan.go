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
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// clusteredIDRange returns the primary key range [start, stop) that the _id
// condition of filter bounds. An $in is bounded by its least and greatest
// values, as MongoDB scans a clustered collection. ok is false once ctx ends.
func clusteredIDRange(ctx context.Context, filter *types.Document) (start, stop []byte, ok bool) {
	if filter == nil {
		return nil, nil, false
	}
	if id, ok := simpleIDEquality(filter); ok {
		key, err := primaryKey(id)
		if err != nil {
			return nil, nil, false
		}
		return key, keySuccessor(key), true
	}
	v, err := filter.Get("_id")
	if err != nil {
		return nil, nil, false
	}
	ops, isDoc := v.(*types.Document)
	if isDoc && ops.Len() == 1 && ops.Keys()[0] == "$eq" {
		v, _ = ops.Get("$eq")
	}
	if v == types.Null {
		// Starts at undefined, which sorts just before null, so the
		// downstream filter decides whether null matches it.
		return idxpkg.EncodeValue(types.Undefined), keySuccessor(idxpkg.EncodeValue(types.Null)), true
	}
	if !isDoc {
		return nil, nil, false
	}
	if in, err := ops.Get("$in"); err == nil && ops.Len() == 1 {
		return clusteredInRange(ctx, in)
	}
	start, stop, ok = indexBoundsForFilterValue(v)
	if !ok {
		return nil, nil, false
	}
	return idxpkg.ValueBound(start), idxpkg.ValueBound(stop), true
}

func clusteredInRange(ctx context.Context, in any) (start, stop []byte, ok bool) {
	arr, isArr := in.(*types.Array)
	if !isArr || arr.Len() == 0 {
		return nil, nil, false
	}
	var lo, hi []byte
	for i := 0; i < arr.Len(); i++ {
		// Encoding a small Decimal128 can take hundreds of microseconds, so a
		// large $in checks for maxTimeMS and disconnects as it goes.
		if i%256 == 255 && ctx.Err() != nil {
			return nil, nil, false
		}
		v, err := arr.Get(i)
		if err != nil {
			return nil, nil, false
		}
		switch v.(type) {
		case nil, types.NullType, *types.Array, types.Regex:
			return nil, nil, false
		}
		if doc, isDoc := v.(*types.Document); isDoc && doc.Len() > 0 && strings.HasPrefix(doc.Keys()[0], "$") {
			return nil, nil, false
		}
		key, err := primaryKey(v)
		if err != nil {
			return nil, nil, false
		}
		if lo == nil || bytes.Compare(key, lo) < 0 {
			lo = key
		}
		if hi == nil || bytes.Compare(key, hi) > 0 {
			hi = key
		}
	}
	return lo, keySuccessor(hi), true
}

// keySuccessor is the least key past key; value encodings are prefix-free.
func keySuccessor(key []byte) []byte {
	return append(bytes.Clone(key), 0x00)
}

// sortIsID reports whether sort is {_id: 1} or {_id: -1}, and which.
func sortIsID(sort *types.Document) (isID, descending bool) {
	if sort == nil || sort.Len() != 1 || sort.Keys()[0] != "_id" {
		return false, false
	}
	return true, !sortDirectionAscending(sort)
}

// clusteredScan reads the primary map over [start, stop) in key order, or
// reverse key order, from either bound when it is nil. Like MongoDB's clustered
// scan, it counts the document past the far bound as examined.
func clusteredScan(ctx context.Context, state *dbState, m prolly.Map, start, stop []byte, reverse bool, params *backends.QueryParams) (types.DocumentsIterator, error) {
	var iter prolly.MapIter
	var err error
	if reverse {
		iter, err = newReverseKeyIter(ctx, m, stop)
	} else {
		var ok bool
		if iter, ok, err = idxpkg.SeekKeyRange(ctx, m, start, nil); err == nil && !ok {
			return newEmptyIter(), nil
		}
	}
	if err != nil {
		return nil, err
	}
	mi := &mapIter{ctx: ctx, ns: state.ns, iter: iter, limit: params.Limit, onlyRecordID: params.OnlyRecordIDs}
	if !params.OnlyRecordIDs && !params.Collated {
		mi.prefilter = buildScanPrefilter(params.Filter)
		mi.prefixReject = buildScanPrefixRejecter(params.Filter)
	}
	mi.fields = params.Fields
	mi.lazy = params.LazyDocuments && params.Fields == nil
	switch {
	case reverse && start != nil:
		mi.outside = func(key []byte) bool { return bytes.Compare(key, start) < 0 }
	case !reverse && stop != nil:
		mi.outside = func(key []byte) bool { return bytes.Compare(key, stop) >= 0 }
	}
	if state.backend != nil && state.backend.backgroundRP != nil {
		mi.release = state.backend.backgroundRP.pinRoot(state.name, m.HashOf())
	}
	return mi, nil
}

// reverseKeyIter reads a map's entries before stop, or all of them when stop
// is nil, in reverse key order, a chunk of ordinals at a time.
type reverseKeyIter struct {
	m     prolly.Map
	hi    uint64
	chunk uint64
	buf   [][2]val.Tuple
}

func newReverseKeyIter(ctx context.Context, m prolly.Map, stop []byte) (*reverseKeyIter, error) {
	count, err := m.Count()
	if err != nil {
		return nil, err
	}
	hi := uint64(count)
	if stop != nil {
		key, err := buildKey(stop)
		if err != nil {
			return nil, err
		}
		if hi, err = m.GetOrdinalForKey(ctx, key); err != nil {
			return nil, err
		}
	}
	return &reverseKeyIter{m: m, hi: hi, chunk: 16}, nil
}

func (it *reverseKeyIter) Next(ctx context.Context) (val.Tuple, val.Tuple, error) {
	if len(it.buf) == 0 {
		if it.hi == 0 {
			return nil, nil, io.EOF
		}
		from := it.hi - min(it.hi, it.chunk)
		entries, err := it.m.IterOrdinalRange(ctx, from, it.hi)
		if err != nil {
			return nil, nil, err
		}
		for {
			k, v, err := entries.Next(ctx)
			if err == io.EOF || (err == nil && len(k) == 0) {
				break
			}
			if err != nil {
				return nil, nil, err
			}
			it.buf = append(it.buf, [2]val.Tuple{k, v})
		}
		it.hi = from
		it.chunk = min(it.chunk*2, 1024)
		if len(it.buf) == 0 {
			return nil, nil, io.EOF
		}
	}
	last := it.buf[len(it.buf)-1]
	it.buf = it.buf[:len(it.buf)-1]
	return last[0], last[1], nil
}

// clusteredQuery scans the primary map over the _id range params.Filter
// bounds, in the order of an _id sort. ok is false when params has neither.
func clusteredQuery(ctx context.Context, state *dbState, m prolly.Map, params *backends.QueryParams) (*backends.QueryResult, bool, error) {
	idSort, descending := sortIsID(params.Sort)
	start, stop, ranged := clusteredIDRange(ctx, params.Filter)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !ranged && !idSort {
		return nil, false, nil
	}
	iter, err := clusteredScan(ctx, state, m, start, stop, descending, params)
	if err != nil {
		return nil, false, err
	}
	return &backends.QueryResult{Iter: iter, Sorted: idSort}, true, nil
}
