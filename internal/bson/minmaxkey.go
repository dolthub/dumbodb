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

package bson

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

const (
	bsonTagMinKey byte = 0xFF
	bsonTagMaxKey byte = 0x7F
	bsonTagNull   byte = 0x0A
)

// hasMinMaxKey returns true if the raw BSON document contains MinKey or MaxKey at the top level.
func hasMinMaxKey(raw []byte) bool {
	if len(raw) < 5 {
		return false
	}
	offset := 4
	for {
		if offset >= len(raw) {
			return false
		}
		t := raw[offset]
		if t == 0 {
			return false
		}
		if t == bsonTagMinKey || t == bsonTagMaxKey {
			return true
		}
		offset++
		// Skip key (null-terminated string)
		for offset < len(raw) && raw[offset] != 0 {
			offset++
		}
		offset++ // null terminator
		// Skip value
		n, err := rawBSONValueSize(raw[offset:], t)
		if err != nil {
			return false
		}
		offset += n
	}
}

// patchMinMaxKeyInRawBSON creates a copy of raw with MinKey/MaxKey type tags replaced by Null (0x0A).
// It also returns the field names of replaced MinKey and MaxKey entries.
// Only top-level fields are patched; nested documents are not affected.
func patchMinMaxKeyInRawBSON(raw []byte) (patched []byte, minKeyFields, maxKeyFields []string, err error) {
	patched = make([]byte, len(raw))
	copy(patched, raw)

	offset := 4
	for {
		if offset >= len(patched) {
			return nil, nil, nil, fmt.Errorf("bson: unexpected end of document")
		}
		t := patched[offset]
		if t == 0 {
			break
		}

		isMinKey := t == bsonTagMinKey
		isMaxKey := t == bsonTagMaxKey
		if isMinKey || isMaxKey {
			patched[offset] = bsonTagNull
		}
		offset++ // advance past type byte

		// Read key
		keyStart := offset
		for offset < len(patched) && patched[offset] != 0 {
			offset++
		}
		if offset >= len(patched) {
			return nil, nil, nil, fmt.Errorf("bson: unterminated key")
		}
		key := string(patched[keyStart:offset])
		offset++ // null terminator

		if isMinKey {
			minKeyFields = append(minKeyFields, key)
			// Null has no value bytes; no advance needed.
		} else if isMaxKey {
			maxKeyFields = append(maxKeyFields, key)
			// Null has no value bytes; no advance needed.
		} else {
			n, err := rawBSONValueSize(patched[offset:], t)
			if err != nil {
				return nil, nil, nil, err
			}
			offset += n
		}
	}

	return patched, minKeyFields, maxKeyFields, nil
}

// rawBSONValueSize returns the number of bytes occupied by a BSON value of the given type tag,
// starting at the beginning of data. For variable-length types it reads the length prefix.
func rawBSONValueSize(data []byte, t byte) (int, error) {
	switch t {
	case 0x01: // float64
		return 8, nil
	case 0x02: // string
		if len(data) < 4 {
			return 0, fmt.Errorf("bson: short string length")
		}
		n := int(binary.LittleEndian.Uint32(data))
		return 4 + n, nil
	case 0x03, 0x04: // document, array
		if len(data) < 4 {
			return 0, fmt.Errorf("bson: short doc/array length")
		}
		n := int(binary.LittleEndian.Uint32(data))
		return n, nil
	case 0x05: // binary
		if len(data) < 4 {
			return 0, fmt.Errorf("bson: short binary length")
		}
		n := int(binary.LittleEndian.Uint32(data))
		return 4 + 1 + n, nil
	case 0x06: // undefined (deprecated)
		return 0, nil
	case 0x07: // ObjectID
		return 12, nil
	case 0x08: // bool
		return 1, nil
	case 0x09: // datetime
		return 8, nil
	case 0x0A: // null
		return 0, nil
	case 0x0B: // regex — two null-terminated strings
		n := 0
		for n < len(data) && data[n] != 0 {
			n++
		}
		n++ // first null terminator
		for n < len(data) && data[n] != 0 {
			n++
		}
		n++ // second null terminator
		return n, nil
	case 0x10: // int32
		return 4, nil
	case 0x11: // timestamp
		return 8, nil
	case 0x12: // int64
		return 8, nil
	case 0x13: // Decimal128
		return 16, nil
	case 0x7F: // MaxKey
		return 0, nil
	case 0xFF: // MinKey
		return 0, nil
	default:
		return 0, fmt.Errorf("bson: unknown type tag 0x%02X", t)
	}
}

// ToDocumentHandlingMinMaxKey converts a wirebson document to types.Document,
// handling MinKey and MaxKey which the wirebson library doesn't support.
// Falls back to the normal ToDocument path when no MinKey/MaxKey are present.
func ToDocumentHandlingMinMaxKey(d wirebson.AnyDocument) (*types.Document, error) {
	raw, err := d.Encode()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if !hasMinMaxKey([]byte(raw)) {
		return nil, nil // signal: use normal path
	}

	patched, minKeyFields, maxKeyFields, err := patchMinMaxKeyInRawBSON([]byte(raw))
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Decode the patched document (MinKey/MaxKey replaced with Null) using normal path.
	result, err := ToDocument(wirebson.RawDocument(patched))
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Restore MinKey/MaxKey values.
	for _, key := range minKeyFields {
		result.Set(key, types.MinKey)
	}
	for _, key := range maxKeyFields {
		result.Set(key, types.MaxKey)
	}

	return result, nil
}

// FromDocumentRaw encodes a types.Document to raw BSON bytes, handling MinKey and MaxKey.
// For documents without MinKey/MaxKey, prefer FromDocument which uses wirebson.
func FromDocumentRaw(doc *types.Document) ([]byte, error) {
	if doc == nil {
		panic("bson.FromDocumentRaw: doc is nil")
	}

	var buf bytes.Buffer
	// Reserve 4 bytes for the document length.
	buf.Write([]byte{0, 0, 0, 0})

	iter := doc.Iterator()
	defer iter.Close()

	for {
		k, v, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			return nil, lazyerrors.Error(err)
		}

		switch val := v.(type) {
		case types.MinKeyType:
			buf.WriteByte(bsonTagMinKey)
			writeRawCString(&buf, k)
			// No value bytes.
		case types.MaxKeyType:
			buf.WriteByte(bsonTagMaxKey)
			writeRawCString(&buf, k)
			// No value bytes.
		default:
			// Use wirebson for all other types.
			wDoc := wirebson.MakeDocument(1)
			wv, err := convertFromTypes(val)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			if err := wDoc.Add(k, wv); err != nil {
				return nil, lazyerrors.Error(err)
			}
			encoded, err := wDoc.Encode()
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			// Extract just the field from the encoded document:
			// Skip 4-byte length, extract until 0x00 terminator.
			fieldBytes := encoded[4 : len(encoded)-1]
			buf.Write(fieldBytes)
		}
	}

	// Write terminator.
	buf.WriteByte(0)

	result := buf.Bytes()
	// Write length prefix (4 bytes, little-endian).
	binary.LittleEndian.PutUint32(result, uint32(len(result)))
	return result, nil
}

// writeRawCString writes a null-terminated string to buf.
func writeRawCString(buf *bytes.Buffer, s string) {
	buf.WriteString(s)
	buf.WriteByte(0)
}
