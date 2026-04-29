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

// EqualityLookup scans the secondary index for entries where the indexed field
// has the given value. It returns the primary key bytes (encodedID) for each match.
//
// For each matching index entry, the primary key bytes can be used to look up the
// document in the primary prolly.Map.
func EqualityLookup(ctx context.Context, m prolly.Map, fieldValue any) ([][]byte, error) {
	// Build the prefix: KeyString(fieldValue) + discriminator.
	prefix := append(EncodeValue(fieldValue), discriminator)

	iter, err := m.IterAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("index: iterating secondary index: %w", err)
	}

	var results [][]byte
	for {
		k, _, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("index: reading secondary index: %w", err)
		}
		if k == nil {
			break
		}

		compositeKey, ok := idxKeyDesc.GetBytes(0, k)
		if !ok {
			continue
		}

		// Check if the key starts with our prefix.
		if !bytes.HasPrefix(compositeKey, prefix) {
			continue
		}

		// Extract the primary ID bytes: everything after the prefix.
		primaryIDBytes := compositeKey[len(prefix):]
		if len(primaryIDBytes) == 0 {
			continue
		}

		// Copy since the underlying buffer may be reused.
		idCopy := make([]byte, len(primaryIDBytes))
		copy(idCopy, primaryIDBytes)
		results = append(results, idCopy)
	}

	return results, nil
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
