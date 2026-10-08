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
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// DecodeRawDocument decodes a BSON document directly into [*types.Document],
// following the same validation rules as [ToDocument] without building an
// intermediate wirebson document. MinKey and MaxKey decode at any depth.
//
// Input nested deeper than [MaxNestingDepth] returns [ErrNestingTooDeep].
func DecodeRawDocument(raw []byte) (*types.Document, error) {
	return decodeRawDocument(raw, 1)
}

func decodeRawDocument(raw []byte, depth int) (*types.Document, error) {
	if depth > MaxNestingDepth {
		return nil, ErrNestingTooDeep
	}
	pairs, err := decodeRawFields(raw, false, depth)
	if err != nil {
		return nil, err
	}
	return types.NewDocument(pairs...)
}

func decodeRawArray(raw []byte, depth int) (*types.Array, error) {
	if depth > MaxNestingDepth {
		return nil, ErrNestingTooDeep
	}
	pairs, err := decodeRawFields(raw, true, depth)
	if err != nil {
		return nil, err
	}
	values := make([]any, len(pairs)/2)
	for i := range values {
		values[i] = pairs[2*i+1]
	}
	return types.NewArray(values...)
}

// decodeRawFields returns alternating names and values. For arrays, names
// must be the element indexes in order.
func decodeRawFields(raw []byte, isArray bool, depth int) ([]any, error) {
	if err := checkRawLength(raw); err != nil {
		return nil, err
	}

	var pairs []any
	offset := 4
	for {
		if offset >= len(raw) {
			return nil, fmt.Errorf("bson: unexpected end of document at offset %d", offset)
		}
		t := raw[offset]
		if t == 0 {
			if offset != len(raw)-1 {
				return nil, fmt.Errorf("bson: document terminator at offset %d of %d bytes", offset, len(raw))
			}
			return pairs, nil
		}
		offset++

		end := bytes.IndexByte(raw[offset:], 0)
		if end < 0 {
			return nil, fmt.Errorf("bson: unterminated field name at offset %d", offset)
		}
		name := string(raw[offset : offset+end])
		offset += end + 1

		if isArray && name != strconv.Itoa(len(pairs)/2) {
			return nil, fmt.Errorf("bson: invalid array index %q", name)
		}

		v, n, err := decodeRawValue(raw[offset:], t, depth)
		if err != nil {
			return nil, fmt.Errorf("bson: field %q: %w", name, err)
		}
		offset += n
		pairs = append(pairs, name, v)
	}
}

func checkRawLength(raw []byte) error {
	if len(raw) < 5 {
		return fmt.Errorf("bson: document of %d bytes is shorter than 5", len(raw))
	}
	if l := binary.LittleEndian.Uint32(raw); int64(l) != int64(len(raw)) {
		return fmt.Errorf("bson: length prefix %d does not match %d bytes", l, len(raw))
	}
	if raw[len(raw)-1] != 0 {
		return fmt.Errorf("bson: document does not end with 0")
	}
	return nil
}

// embeddedLength validates the length prefix of an embedded document or array
// at the start of b.
func embeddedLength(b []byte) (int, error) {
	if len(b) < 5 {
		return 0, fmt.Errorf("embedded document of %d bytes is shorter than 5", len(b))
	}
	l := int64(binary.LittleEndian.Uint32(b))
	if l < 5 || l > int64(len(b)) {
		return 0, fmt.Errorf("embedded length %d out of range for %d bytes", l, len(b))
	}
	return int(l), nil
}

func decodeRawValue(b []byte, t byte, depth int) (any, int, error) {
	fixed := func(n int) error {
		if len(b) < n {
			return fmt.Errorf("value of tag 0x%02x needs %d bytes, got %d", t, n, len(b))
		}
		return nil
	}

	switch t {
	case 0x01:
		if err := fixed(8); err != nil {
			return nil, 0, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), 8, nil

	case 0x02:
		if err := fixed(5); err != nil {
			return nil, 0, err
		}
		l := int64(int32(binary.LittleEndian.Uint32(b)))
		if l < 1 {
			return nil, 0, fmt.Errorf("string length prefix %d is less than 1", l)
		}
		if 4+l > int64(len(b)) {
			return nil, 0, fmt.Errorf("string of %d bytes exceeds %d remaining", l, len(b)-4)
		}
		if b[4+l-1] != 0 {
			return nil, 0, fmt.Errorf("string does not end with 0")
		}
		return string(b[4 : 4+l-1]), int(4 + l), nil

	case 0x03:
		l, err := embeddedLength(b)
		if err != nil {
			return nil, 0, err
		}
		doc, err := decodeRawDocument(b[:l], depth+1)
		return doc, l, err

	case 0x04:
		l, err := embeddedLength(b)
		if err != nil {
			return nil, 0, err
		}
		arr, err := decodeRawArray(b[:l], depth+1)
		return arr, l, err

	case 0x05:
		if err := fixed(5); err != nil {
			return nil, 0, err
		}
		l := int64(binary.LittleEndian.Uint32(b))
		if 5+l > int64(len(b)) {
			return nil, 0, fmt.Errorf("binary of %d bytes exceeds %d remaining", l, len(b)-5)
		}
		data := make([]byte, l)
		copy(data, b[5:5+l])
		return types.Binary{B: data, Subtype: types.BinarySubtype(b[4])}, int(5 + l), nil

	case 0x07:
		if err := fixed(types.ObjectIDLen); err != nil {
			return nil, 0, err
		}
		var id types.ObjectID
		copy(id[:], b)
		return id, types.ObjectIDLen, nil

	case 0x08:
		if err := fixed(1); err != nil {
			return nil, 0, err
		}
		switch b[0] {
		case 0x00:
			return false, 1, nil
		case 0x01:
			return true, 1, nil
		default:
			return nil, 0, fmt.Errorf("bool byte 0x%02x is not 0x00 or 0x01", b[0])
		}

	case 0x09:
		if err := fixed(8); err != nil {
			return nil, 0, err
		}
		return time.UnixMilli(int64(binary.LittleEndian.Uint64(b))).UTC(), 8, nil

	case 0x0A:
		return types.Null, 0, nil

	case 0x0B:
		p := bytes.IndexByte(b, 0)
		if p < 0 {
			return nil, 0, fmt.Errorf("regex pattern is unterminated")
		}
		o := bytes.IndexByte(b[p+1:], 0)
		if o < 0 {
			return nil, 0, fmt.Errorf("regex options are unterminated")
		}
		o += p + 1
		return types.Regex{Pattern: string(b[:p]), Options: string(b[p+1 : o])}, o + 1, nil

	case 0x10:
		if err := fixed(4); err != nil {
			return nil, 0, err
		}
		return int32(binary.LittleEndian.Uint32(b)), 4, nil

	case 0x11:
		if err := fixed(8); err != nil {
			return nil, 0, err
		}
		return types.Timestamp(binary.LittleEndian.Uint64(b)), 8, nil

	case 0x12:
		if err := fixed(8); err != nil {
			return nil, 0, err
		}
		return int64(binary.LittleEndian.Uint64(b)), 8, nil

	case 0x13:
		if err := fixed(16); err != nil {
			return nil, 0, err
		}
		return types.Decimal128{L: binary.LittleEndian.Uint64(b), H: binary.LittleEndian.Uint64(b[8:])}, 16, nil

	case bsonTagMinKey:
		return types.MinKey, 0, nil

	case bsonTagMaxKey:
		return types.MaxKey, 0, nil

	default:
		return nil, 0, fmt.Errorf("unsupported BSON tag 0x%02x", t)
	}
}
