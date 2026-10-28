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

package index

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
)

// idxKeyDesc describes the secondary index key tuple: one varbinary field
// holding [KeyString(fieldValue...)][0x04][encodedPrimaryID bytes].
var idxKeyDesc = val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc, Nullable: false})

// idxValDesc describes the secondary index value tuple: a single dummy byte
// (secondary index entries carry no payload; the primary key is in the index key).
var idxValDesc = val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc, Nullable: false})

var idxBufPool = pool.NewBuffPool()

// discriminator is appended between the KeyString-encoded field value and the
// primary key bytes in a secondary index entry key.
const discriminator = byte(0x04)

func NewEmptyMap(ctx context.Context, ns tree.NodeStore) (prolly.Map, error) {
	return prolly.NewMapFromTuples(ctx, ns, idxKeyDesc, idxValDesc)
}

// BuildSecondaryKey encodes an index entry key:
//
//	[KeyString(fieldValues...)][0x04][primaryIDBytes]
func BuildSecondaryKey(fieldValues []any, primaryIDBytes []byte) []byte {
	var buf bytes.Buffer
	for _, v := range fieldValues {
		buf.Write(EncodeValue(v))
	}
	buf.WriteByte(discriminator)
	buf.Write(primaryIDBytes)
	return buf.Bytes()
}

func BuildIndexEntry(fieldValues []any, primaryIDBytes []byte) (val.Tuple, val.Tuple, error) {
	compositeKey := BuildSecondaryKey(fieldValues, primaryIDBytes)

	ktb := val.NewTupleBuilder(idxKeyDesc, nil)
	ktb.PutByteString(0, compositeKey)
	keyTuple, err := ktb.Build(context.Background(), idxBufPool)
	if err != nil {
		return nil, nil, fmt.Errorf("index: building key tuple: %w", err)
	}

	vtb := val.NewTupleBuilder(idxValDesc, nil)
	vtb.PutByteString(0, []byte{})
	valTuple, err := vtb.Build(context.Background(), idxBufPool)
	if err != nil {
		return nil, nil, fmt.Errorf("index: building val tuple: %w", err)
	}

	return keyTuple, valTuple, nil
}

func InsertEntry(ctx context.Context, mut *prolly.MutableMap, fieldValues []any, primaryIDBytes []byte) error {
	keyTuple, valTuple, err := BuildIndexEntry(fieldValues, primaryIDBytes)
	if err != nil {
		return err
	}
	return mut.Put(ctx, keyTuple, valTuple)
}

func DeleteEntry(ctx context.Context, mut *prolly.MutableMap, fieldValues []any, primaryIDBytes []byte) error {
	keyTuple, _, err := BuildIndexEntry(fieldValues, primaryIDBytes)
	if err != nil {
		return err
	}
	return mut.Delete(ctx, keyTuple)
}

// EqualityLookup returns the primary key bytes for each index entry where the
// indexed field equals fieldValue, via a bounded range scan over the contiguous
// block of entries [KeyString(v)+0x04, KeyString(v)+0x05).
func EqualityLookup(ctx context.Context, m prolly.Map, fieldValue any) ([][]byte, error) {
	encoded := EncodeValue(fieldValue)
	startKey := append(append([]byte(nil), encoded...), discriminator)
	stopKey := append(append([]byte(nil), encoded...), discriminator+1)
	return RangeLookup(ctx, m, startKey, stopKey)
}

// EqualityProbeBounds returns the [start, stop) range of entries whose
// value tuple equals fieldValues, regardless of primary ID.
func EqualityProbeBounds(fieldValues []any) (startKey, stopKey []byte) {
	prefix := BuildSecondaryKey(fieldValues, nil) // KS(values) || 0x04
	stop := append([]byte(nil), prefix...)
	stop[len(stop)-1] = discriminator + 1
	return prefix, stop
}

// UniqueConflict reports whether the index holds an entry with this
// value tuple under a primary ID other than selfID.
func UniqueConflict(ctx context.Context, m prolly.Map, fieldValues []any, selfID []byte) (bool, error) {
	start, stop := EqualityProbeBounds(fieldValues)
	ids, _, err := RangeLookupCapped(ctx, m, start, stop, 2)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if !bytes.Equal(id, selfID) {
			return true, nil
		}
	}
	return false, nil
}

// LowerBoundInclusive returns the smallest composite-index key that has
// fieldValue as its leading field. Suitable as the inclusive start of a
// $gte / equality scan.
func LowerBoundInclusive(fieldValue any) []byte {
	out := append(EncodeValue(fieldValue), discriminator)
	return out
}

// LowerBoundExclusive returns the smallest composite-index key that sorts
// strictly after every entry whose leading field equals fieldValue. Suitable
// as the inclusive start of a $gt scan.
func LowerBoundExclusive(fieldValue any) []byte {
	out := append(EncodeValue(fieldValue), discriminator+1)
	return out
}

// UpperBoundExclusive returns the smallest composite-index key whose leading
// field equals fieldValue. Suitable as the exclusive stop of a $lt scan.
func UpperBoundExclusive(fieldValue any) []byte {
	out := append(EncodeValue(fieldValue), discriminator)
	return out
}

// UpperBoundInclusive returns the smallest composite-index key that sorts
// strictly after every entry whose leading field equals fieldValue. Suitable
// as the exclusive stop of a $lte scan.
func UpperBoundInclusive(fieldValue any) []byte {
	out := append(EncodeValue(fieldValue), discriminator+1)
	return out
}

// boundTuple wraps a raw composite-key byte slice in the index's val.Tuple
// shape so it can be passed to prolly.Map.IterKeyRange. A nil/empty slice
// produces a nil tuple, which IterKeyRange interprets as an open bound.
func boundTuple(key []byte) (val.Tuple, error) {
	if len(key) == 0 {
		return nil, nil
	}
	tb := val.NewTupleBuilder(idxKeyDesc, nil)
	tb.PutByteString(0, key)
	t, err := tb.Build(context.Background(), idxBufPool)
	if err != nil {
		return nil, fmt.Errorf("index: building bound tuple: %w", err)
	}
	return t, nil
}

// RangeLookup returns the primary key bytes for each index entry whose
// composite key falls in [startKey, stopKey). A nil or empty bound is open
// on that side. Iteration uses prolly.Map.IterKeyRange so only the relevant
// chunks of the secondary index are touched.
func RangeLookup(ctx context.Context, m prolly.Map, startKey, stopKey []byte) ([][]byte, error) {
	results, _, _, err := rangeLookupCapped(ctx, m, startKey, stopKey, -1, false)
	return results, err
}

// RangeLookupCapped is like RangeLookup but aborts as soon as the result count
// exceeds maxResults. The caller signals "no cap" with maxResults < 0. When
// the cap is exceeded, exceeded is true and the (partial) results slice is
// returned for the caller to discard  -- no further index work is done.
//
// The cap exists so a low-selectivity filter (e.g. {$gte: 0} that matches
// every document) doesn't pay the full scan-then-point-fetch cost just to
// end up worse than a sequential primary scan.
func RangeLookupCapped(ctx context.Context, m prolly.Map, startKey, stopKey []byte, maxResults int) (results [][]byte, exceeded bool, err error) {
	results, _, exceeded, err = rangeLookupCapped(ctx, m, startKey, stopKey, maxResults, false)
	return results, exceeded, err
}

// RangeEntriesCapped is RangeLookupCapped also returning, for each id, the
// entry's encoded index values (its key without the trailing id).
func RangeEntriesCapped(ctx context.Context, m prolly.Map, startKey, stopKey []byte, maxResults int) (ids, values [][]byte, exceeded bool, err error) {
	return rangeLookupCapped(ctx, m, startKey, stopKey, maxResults, true)
}

func rangeLookupCapped(ctx context.Context, m prolly.Map, startKey, stopKey []byte, maxResults int, keepValues bool) ([][]byte, [][]byte, bool, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return nil, nil, false, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return nil, nil, false, err
	}

	iter, err := m.IterKeyRange(ctx, startTup, stopTup)
	if err != nil {
		return nil, nil, false, fmt.Errorf("index: iterating secondary index range: %w", err)
	}

	var results, values [][]byte
	for {
		k, _, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, nil, false, fmt.Errorf("index: reading secondary index: %w", err)
		}
		if k == nil {
			break
		}

		compositeKey, ok := idxKeyDesc.GetBytes(0, k)
		if !ok {
			continue
		}

		// The primary ID byte width is fixed at 20 (see hashID in the dolt
		// backend), so the primary ID is unambiguously the last 20 bytes of
		// the composite key. We can't scan from the left for the discriminator
		// because 0x04 may legitimately appear inside KeyString-encoded
		// values (e.g. as a single-byte integer payload).
		const primaryIDLen = 20
		if len(compositeKey) < primaryIDLen+1 {
			continue
		}
		idStart := len(compositeKey) - primaryIDLen
		if compositeKey[idStart-1] != discriminator {
			continue
		}

		idCopy := make([]byte, primaryIDLen)
		copy(idCopy, compositeKey[idStart:])
		results = append(results, idCopy)
		if keepValues {
			values = append(values, bytes.Clone(compositeKey[:idStart-1]))
		}

		if maxResults >= 0 && len(results) > maxResults {
			return nil, nil, true, nil
		}
	}

	return results, values, false, nil
}

// RangeCount returns the number of secondary index entries whose composite
// key falls in [startKey, stopKey). A nil or empty bound is open on that side.
// Iteration uses prolly.Map.IterKeyRange, but unlike RangeLookup this does not
// extract or copy the primary key bytes  -- it only counts entries, so the cost
// is the index walk itself with no per-entry primary fetch.
func RangeCount(ctx context.Context, m prolly.Map, startKey, stopKey []byte) (int64, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return 0, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return 0, err
	}

	iter, err := m.IterKeyRange(ctx, startTup, stopTup)
	if err != nil {
		return 0, fmt.Errorf("index: iterating secondary index range: %w", err)
	}

	var n int64
	for {
		k, _, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("index: reading secondary index: %w", err)
		}
		if k == nil {
			break
		}
		n++
	}

	return n, nil
}

// KeyDescriptor returns the key tuple descriptor used for secondary index maps.
// Exposed so the dolt backend can open persisted secondary index maps.
func KeyDescriptor() *val.TupleDesc {
	return idxKeyDesc
}

// ValDescriptor returns the value tuple descriptor used for secondary index maps.
func ValDescriptor() *val.TupleDesc {
	return idxValDesc
}

// RangeIDIter yields the primary ids of the entries in [startKey, stopKey),
// reading the index only as far as the caller consumes.
type RangeIDIter struct {
	m        prolly.Map
	reverse  bool
	entries  prolly.MapIter
	lo, hi   uint64
	chunk    uint64
	buffered [][]byte
}

// NewRangeIDIter iterates [startKey, stopKey) forward, or backward when
// reverse. A nil or empty bound is open on that side.
func NewRangeIDIter(ctx context.Context, m prolly.Map, startKey, stopKey []byte, reverse bool) (*RangeIDIter, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return nil, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return nil, err
	}
	it := &RangeIDIter{m: m, reverse: reverse}
	if !reverse {
		it.entries, err = m.IterKeyRange(ctx, startTup, stopTup)
		return it, err
	}
	if startTup != nil {
		if it.lo, err = m.GetOrdinalForKey(ctx, startTup); err != nil {
			return nil, err
		}
	}
	if stopTup != nil {
		it.hi, err = m.GetOrdinalForKey(ctx, stopTup)
	} else {
		var n int
		n, err = m.Count()
		it.hi = uint64(n)
	}
	it.chunk = 16
	return it, err
}

// Next returns the next primary id, or nil at the end of the range.
func (it *RangeIDIter) Next(ctx context.Context) ([]byte, error) {
	for {
		if it.reverse {
			if len(it.buffered) > 0 {
				id := it.buffered[len(it.buffered)-1]
				it.buffered = it.buffered[:len(it.buffered)-1]
				return id, nil
			}
			if it.hi <= it.lo {
				return nil, nil
			}
			if err := it.fillBackward(ctx); err != nil {
				return nil, err
			}
			continue
		}
		k, _, err := it.entries.Next(ctx)
		if err == io.EOF || (err == nil && k == nil) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("index: reading secondary index: %w", err)
		}
		if id, ok := entryPrimaryID(k); ok {
			return id, nil
		}
	}
}

// fillBackward buffers the ids of the chunk of entries ending at hi, doubling
// the chunk each time up to 1024.
func (it *RangeIDIter) fillBackward(ctx context.Context) error {
	from := it.lo
	if it.hi-it.lo > it.chunk {
		from = it.hi - it.chunk
	}
	entries, err := it.m.IterOrdinalRange(ctx, from, it.hi)
	if err != nil {
		return err
	}
	for {
		k, _, err := entries.Next(ctx)
		if err == io.EOF || (err == nil && k == nil) {
			break
		}
		if err != nil {
			return fmt.Errorf("index: reading secondary index: %w", err)
		}
		if id, ok := entryPrimaryID(k); ok {
			it.buffered = append(it.buffered, id)
		}
	}
	it.hi = from
	it.chunk = min(it.chunk*2, 1024)
	return nil
}

func entryPrimaryID(k val.Tuple) ([]byte, bool) {
	const primaryIDLen = 20
	compositeKey, ok := idxKeyDesc.GetBytes(0, k)
	if !ok || len(compositeKey) < primaryIDLen+1 {
		return nil, false
	}
	idStart := len(compositeKey) - primaryIDLen
	if compositeKey[idStart-1] != discriminator {
		return nil, false
	}
	return bytes.Clone(compositeKey[idStart:]), true
}

// IterKeyRange iterates the entries in [startKey, stopKey); a nil or empty
// bound is open on that side.
func IterKeyRange(ctx context.Context, m prolly.Map, startKey, stopKey []byte) (prolly.MapIter, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return nil, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return nil, err
	}
	return m.IterKeyRange(ctx, startTup, stopTup)
}

// RangeCardinality returns the number of entries in [startKey, stopKey)
// without reading them; a nil or empty bound is open on that side.
func RangeCardinality(ctx context.Context, m prolly.Map, startKey, stopKey []byte) (uint64, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return 0, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return 0, err
	}
	return m.GetKeyRangeCardinality(ctx, startTup, stopTup)
}
