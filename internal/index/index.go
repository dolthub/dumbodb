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
// holding [KeyString(fieldValue…)][0x04][encodedPrimaryID bytes].
var idxKeyDesc = val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc, Nullable: false})

// idxValDesc describes the secondary index value tuple: a single dummy byte
// (secondary index entries carry no payload; the primary key is in the index key).
var idxValDesc = val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc, Nullable: false})

var idxBufPool = pool.NewBuffPool()

// discriminator is appended between the KeyString-encoded field value and the
// primary key bytes in a secondary index entry key.
const discriminator = byte(0x04)

// NewEmptyMap creates an empty secondary index prolly.Map.
func NewEmptyMap(ctx context.Context, ns tree.NodeStore) (prolly.Map, error) {
	return prolly.NewMapFromTuples(ctx, ns, idxKeyDesc, idxValDesc)
}

// BuildSecondaryKey encodes an index entry key:
//
//	[KeyString(fieldValues...)][0x04][primaryIDBytes]
//
// fieldValues is the slice of field values extracted from the indexed document.
// primaryIDBytes is the encoded primary key (_id) of the document.
func BuildSecondaryKey(fieldValues []any, primaryIDBytes []byte) []byte {
	var buf bytes.Buffer
	for _, v := range fieldValues {
		buf.Write(EncodeValue(v))
	}
	buf.WriteByte(discriminator)
	buf.Write(primaryIDBytes)
	return buf.Bytes()
}

// BuildIndexEntry creates the key and (empty) value tuples for a secondary index entry.
func BuildIndexEntry(fieldValues []any, primaryIDBytes []byte) (val.Tuple, val.Tuple, error) {
	compositeKey := BuildSecondaryKey(fieldValues, primaryIDBytes)

	ktb := val.NewTupleBuilder(idxKeyDesc, nil)
	ktb.PutByteString(0, compositeKey)
	keyTuple, err := ktb.Build(idxBufPool)
	if err != nil {
		return nil, nil, fmt.Errorf("index: building key tuple: %w", err)
	}

	vtb := val.NewTupleBuilder(idxValDesc, nil)
	vtb.PutByteString(0, []byte{})
	valTuple, err := vtb.Build(idxBufPool)
	if err != nil {
		return nil, nil, fmt.Errorf("index: building val tuple: %w", err)
	}

	return keyTuple, valTuple, nil
}

// InsertEntry inserts a single entry into a mutable secondary index map.
func InsertEntry(ctx context.Context, mut *prolly.MutableMap, fieldValues []any, primaryIDBytes []byte) error {
	keyTuple, valTuple, err := BuildIndexEntry(fieldValues, primaryIDBytes)
	if err != nil {
		return err
	}
	return mut.Put(ctx, keyTuple, valTuple)
}

// DeleteEntry removes a single entry from a mutable secondary index map.
func DeleteEntry(ctx context.Context, mut *prolly.MutableMap, fieldValues []any, primaryIDBytes []byte) error {
	keyTuple, _, err := BuildIndexEntry(fieldValues, primaryIDBytes)
	if err != nil {
		return err
	}
	return mut.Delete(ctx, keyTuple)
}

// EqualityLookup returns the primary key bytes for each index entry where the
// indexed field equals fieldValue. Implemented as a bounded range scan over
// the contiguous block of entries [KeyString(v)+0x04, KeyString(v)+0x05).
func EqualityLookup(ctx context.Context, m prolly.Map, fieldValue any) ([][]byte, error) {
	encoded := EncodeValue(fieldValue)
	startKey := append(append([]byte(nil), encoded...), discriminator)
	stopKey := append(append([]byte(nil), encoded...), discriminator+1)
	return RangeLookup(ctx, m, startKey, stopKey)
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
	t, err := tb.Build(idxBufPool)
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
	results, _, err := rangeLookupCapped(ctx, m, startKey, stopKey, -1)
	return results, err
}

// RangeLookupCapped is like RangeLookup but aborts as soon as the result count
// exceeds maxResults. The caller signals "no cap" with maxResults < 0. When
// the cap is exceeded, exceeded is true and the (partial) results slice is
// returned for the caller to discard — no further index work is done.
//
// The cap exists so a low-selectivity filter (e.g. {$gte: 0} that matches
// every document) doesn't pay the full scan-then-point-fetch cost just to
// end up worse than a sequential primary scan.
func RangeLookupCapped(ctx context.Context, m prolly.Map, startKey, stopKey []byte, maxResults int) (results [][]byte, exceeded bool, err error) {
	return rangeLookupCapped(ctx, m, startKey, stopKey, maxResults)
}

func rangeLookupCapped(ctx context.Context, m prolly.Map, startKey, stopKey []byte, maxResults int) ([][]byte, bool, error) {
	startTup, err := boundTuple(startKey)
	if err != nil {
		return nil, false, err
	}
	stopTup, err := boundTuple(stopKey)
	if err != nil {
		return nil, false, err
	}

	iter, err := m.IterKeyRange(ctx, startTup, stopTup)
	if err != nil {
		return nil, false, fmt.Errorf("index: iterating secondary index range: %w", err)
	}

	var results [][]byte
	for {
		k, _, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, false, fmt.Errorf("index: reading secondary index: %w", err)
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

		if maxResults >= 0 && len(results) > maxResults {
			return nil, true, nil
		}
	}

	return results, false, nil
}

// RangeCount returns the number of secondary index entries whose composite
// key falls in [startKey, stopKey). A nil or empty bound is open on that side.
// Iteration uses prolly.Map.IterKeyRange, but unlike RangeLookup this does not
// extract or copy the primary key bytes — it only counts entries, so the cost
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
