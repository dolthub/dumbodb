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
	"encoding/binary"
	"math"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// Type tags, stored one per indexed value in an entry's value tuple, carry
// what the KeyString encoding drops, as MongoDB's TypeBits do: numbers of
// every type share one encoding, and some values cannot be decoded at all.
const (
	// TagFromKey decodes the value from its encoding alone.
	TagFromKey = byte(0)
	TagInt32   = byte(1)
	TagInt64   = byte(2)
	TagDouble  = byte(3)
	// TagNegZero is the double -0.0, which encodes as zero.
	TagNegZero = byte(4)
	// TagFetch marks a value its encoding cannot restore: a subdocument or
	// array marker, a collation sort key, or a lossy value.
	TagFetch = byte(0xFF)
)

// TypeTags returns the tag of each of values.
func TypeTags(values []any) []byte {
	tags := make([]byte, len(values))
	for i, v := range values {
		tags[i] = typeTag(v)
	}
	return tags
}

func typeTag(v any) byte {
	switch v := v.(type) {
	case int32:
		return TagInt32
	case int64:
		return TagInt64
	case float64:
		if v == 0 && math.Signbit(v) {
			return TagNegZero
		}
		return TagDouble
	case nil, types.NullType, bool, string, types.ObjectID, time.Time, types.Binary,
		types.Timestamp, types.Regex, types.MaxKeyType, types.MinKeyType, types.UndefinedType:
		return TagFromKey
	}
	// Decimal128 shares numbers' encoding without a decoder here; subdocuments
	// and arrays would need a tag per nested number.
	return TagFetch
}

// DecodeValue returns the value enc, one value's KeyString encoding, holds
// under tag, or false when it cannot be restored.
func DecodeValue(enc []byte, tag byte) (any, bool) {
	if l, ok := valueLen(enc); !ok || l != len(enc) || tag == TagFetch {
		return nil, false
	}
	switch tag {
	case TagInt32, TagInt64, TagDouble, TagNegZero:
		return decodeNumber(enc, tag)
	case TagFromKey:
	default:
		return nil, false
	}
	switch c := enc[0]; c {
	case ctypeNull:
		return types.Null, true
	case ctypeUndefined:
		return types.Undefined, true
	case ctypeMinKey:
		return types.MinKey, true
	case ctypeMaxKey:
		return types.MaxKey, true
	case ctypeBoolFalse:
		return false, true
	case ctypeBoolTrue:
		return true, true
	case ctypeString:
		s, _, ok := unescape(enc[1:])
		return string(s), ok
	case ctypeOID:
		var id types.ObjectID
		copy(id[:], enc[1:])
		return id, true
	case ctypeDate:
		return time.UnixMilli(int64(binary.BigEndian.Uint64(enc[1:]) ^ (1 << 63))).UTC(), true
	case ctypeTimestamp:
		return types.Timestamp(binary.BigEndian.Uint64(enc[1:])), true
	case ctypeBinData:
		body := enc[3:]
		if enc[1] == 0xFF {
			body = enc[7:]
		}
		return types.Binary{Subtype: types.BinarySubtype(enc[len(enc)-len(body)-1]), B: append([]byte{}, body...)}, true
	case ctypeRegex:
		pattern, rest, _ := cutCString(enc[1:])
		options, _, _ := cutCString(rest)
		return types.Regex{Pattern: string(pattern), Options: string(options)}, true
	}
	return nil, false
}

// unescape reads a 0x00-terminated body whose 0x00 bytes are escaped as
// 0x00 0xFF, returning it and what follows the terminator.
func unescape(b []byte) (body, rest []byte, ok bool) {
	for i := 0; i < len(b); i++ {
		if b[i] != 0x00 {
			body = append(body, b[i])
			continue
		}
		if i+1 < len(b) && b[i+1] == 0xFF {
			body = append(body, 0x00)
			i++
			continue
		}
		return body, b[i+1:], true
	}
	return nil, nil, false
}

func cutCString(b []byte) (s, rest []byte, ok bool) {
	for i, c := range b {
		if c == 0 {
			return b[:i], b[i+1:], true
		}
	}
	return nil, nil, false
}

// decodeNumber reverses the number encodings of int32, int64 and float64 for
// a number of tag's type. Decimal128-only encodings, which carry a
// continuation or lie beyond the double range, do not decode.
func decodeNumber(enc []byte, tag byte) (any, bool) {
	f, ok := decodeDouble(enc)
	if !ok {
		return nil, false
	}
	switch tag {
	case TagInt32:
		if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
			return nil, false
		}
		return int32(f), true
	case TagInt64:
		n, ok := decodeInt64(enc)
		return n, ok
	case TagNegZero:
		return math.Copysign(0, -1), ok && f == 0
	}
	return f, true
}

func decodeDouble(enc []byte) (float64, bool) {
	c := enc[0]
	negative := c < ctypeZero
	word := func() uint64 {
		w := binary.BigEndian.Uint64(enc[1:9])
		if negative {
			w = ^w
		}
		return w
	}
	sign := func(f float64) float64 {
		if negative {
			return -f
		}
		return f
	}
	switch c {
	case ctypeNumeric:
		return math.NaN(), true
	case ctypeZero:
		return 0, true
	case ctypePosSmall, ctypeNegSmall:
		if len(enc) != 9 {
			return 0, false
		}
		w := word()
		if w < smallBits(0x1p-255) {
			// Scaled: bit 1 holds the double's lowest mantissa bit.
			return sign(math.Float64frombits(w>>1) * 0x1p-768), true
		}
		if w&3 != 0 {
			return 0, false
		}
		return sign(math.Float64frombits(w >> 2)), true
	case ctypePosLarge, ctypeNegLarge:
		if len(enc) != 9 {
			return 0, false
		}
		w := word()
		if w == math.MaxUint64 {
			return sign(math.Inf(1)), true
		}
		if w&1 != 0 || w > largestDoubleBits {
			return 0, false
		}
		return sign(math.Float64frombits((w + 1<<63) >> 1)), true
	}
	n := int(c-ctypePosInt1) + 1
	if negative {
		n = int(ctypeNegInt1-c) + 1
	}
	b := append([]byte(nil), enc[1:]...)
	if negative {
		complement(b)
	}
	var v uint64
	for _, x := range b[:n] {
		v = v<<8 | uint64(x)
	}
	f := float64(v >> 1)
	if v&1 == 1 {
		if len(enc) != 9 || b[7]&3 != 0 {
			return 0, false
		}
		var frac uint64
		for _, x := range b[n:8] {
			frac = frac<<8 | uint64(x)
		}
		f += math.Ldexp(float64(frac), -8*(8-n))
	} else if len(enc) != 1+n {
		return 0, false
	}
	return sign(f), true
}

// decodeInt64 decodes an integral number encoding exactly, past 2^53.
func decodeInt64(enc []byte) (int64, bool) {
	c := enc[0]
	switch {
	case c == ctypeZero:
		return 0, true
	case c == ctypeNegLarge:
		f, ok := decodeDouble(enc)
		return math.MinInt64, ok && f == math.MinInt64
	case c < ctypeNegInt1-7 || c > ctypePosInt1+7 || c == ctypeNegSmall || c == ctypePosSmall:
		return 0, false
	}
	negative := c < ctypeZero
	n := int(c-ctypePosInt1) + 1
	if negative {
		n = int(ctypeNegInt1-c) + 1
	}
	if len(enc) != 1+n {
		return 0, false
	}
	var v uint64
	for _, x := range enc[1 : 1+n] {
		if negative {
			x = ^x
		}
		v = v<<8 | uint64(x)
	}
	if v&1 == 1 {
		return 0, false
	}
	if negative {
		return -int64(v >> 1), true
	}
	return int64(v >> 1), true
}
