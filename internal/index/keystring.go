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

// Package index provides secondary index support for DocuDolt collections.
//
// # KeyString encoding
//
// EncodeValue produces MongoDB's KeyString (V1) bytes for a value, without
// TypeBits: the bytes a mongod clustered collection uses as the RecordId for
// that _id. Byte order is MongoDB's value order, and values MongoDB considers
// equal (1, 1.0, NumberLong(1), NumberDecimal("1")) encode identically. The
// encoding is derived from mongod's observable output and checked against it
// by TestKeyStringMatchesMongod; no MongoDB source is used.
//
// Every encoding delimits itself, so concatenated values split one way.
//
//	MinKey 0x0A, Undefined 0x0F, Null 0x14
//	Numbers 0x1E-0x33: NaN 0x1E; negative large magnitude 0x1F; negative
//	  integer parts of 8..1 bytes 0x20-0x27; negative magnitude below one
//	  0x28; zero 0x29; positive magnitude below one 0x2A; positive integer
//	  parts of 1..8 bytes 0x2B-0x32; positive large magnitude 0x33.
//	  Negative encodings are the bitwise complement of the positive ones.
//	String 0x3C: bytes with 0x00 escaped as 0x00 0xFF, then 0x00
//	Object 0x46: per field [type class][name][0x00][value], then 0x00
//	Array 0x50: element values, then 0x00
//	BinData 0x5A: [length: 1 byte, or 0xFF and 4 bytes][subtype][bytes]
//	ObjectId 0x64, false 0x6E, true 0x6F, Date 0x78, Timestamp 0x82
//	Regex 0x8C: [pattern][0x00][options][0x00]
//	MaxKey 0xF0
package index

import (
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

const (
	ctypeMinKey    = byte(0x0A)
	ctypeUndefined = byte(0x0F)
	ctypeNull      = byte(0x14)
	ctypeNumeric   = byte(0x1E) // type class of every number; also NaN
	ctypeNegLarge  = byte(0x1F)
	ctypeNegInt1   = byte(0x27) // negative integer parts: 0x27 (1 byte) down to 0x20 (8 bytes)
	ctypeNegSmall  = byte(0x28)
	ctypeZero      = byte(0x29)
	ctypePosSmall  = byte(0x2A)
	ctypePosInt1   = byte(0x2B) // positive integer parts: 0x2B (1 byte) up to 0x32 (8 bytes)
	ctypePosLarge  = byte(0x33)
	ctypeString    = byte(0x3C)
	ctypeObject    = byte(0x46)
	ctypeArray     = byte(0x50)
	ctypeBinData   = byte(0x5A)
	ctypeOID       = byte(0x64)
	ctypeBoolFalse = byte(0x6E)
	ctypeBoolTrue  = byte(0x6F)
	ctypeDate      = byte(0x78)
	ctypeTimestamp = byte(0x82)
	ctypeRegex     = byte(0x8C)
	ctypeMaxKey    = byte(0xF0)
)

// PreEncoded is a string body already in comparison-byte form (e.g. an ICU sort
// key); EncodeValue emits it with the normal string layout.
type PreEncoded []byte

// EncodeValue encodes a single BSON value to its KeyString bytes, with the
// fields of embedded documents in canonical order, as documents are stored:
// sorted by name at every level except within a field named _id.
func EncodeValue(v any) []byte {
	return appendValue(nil, v, true)
}

// EncodeID encodes an _id value, keeping its fields in the order given, as
// storage keeps them; for any other value use EncodeValue.
func EncodeID(v any) []byte {
	return appendValue(nil, v, false)
}

func appendValue(out []byte, v any, canonical bool) []byte {
	switch val := v.(type) {
	case nil, types.NullType:
		return append(out, ctypeNull)
	case types.UndefinedType:
		return append(out, ctypeUndefined)
	case types.MinKeyType:
		return append(out, ctypeMinKey)
	case types.MaxKeyType:
		return append(out, ctypeMaxKey)
	case bool:
		if val {
			return append(out, ctypeBoolTrue)
		}
		return append(out, ctypeBoolFalse)
	case int32:
		return appendInt64(out, int64(val))
	case int64:
		return appendInt64(out, val)
	case float64:
		return appendFloat64(out, val)
	case types.Decimal128:
		return appendDecimal128(out, val)
	case string:
		return appendEscaped(append(out, ctypeString), val)
	case PreEncoded:
		return appendEscaped(append(out, ctypeString), string(val))
	case types.ObjectID:
		return append(append(out, ctypeOID), val[:]...)
	case time.Time:
		// Offset binary: signed millisecond order is unsigned byte order.
		return binary.BigEndian.AppendUint64(append(out, ctypeDate), uint64(val.UnixMilli())^(1<<63))
	case types.Timestamp:
		return binary.BigEndian.AppendUint64(append(out, ctypeTimestamp), uint64(val))
	case types.Binary:
		out = append(out, ctypeBinData)
		if n := len(val.B); n < 0xFF {
			out = append(out, byte(n))
		} else {
			out = binary.BigEndian.AppendUint32(append(out, 0xFF), uint32(n))
		}
		return append(append(out, byte(val.Subtype)), val.B...)
	case types.Regex:
		out = append(append(out, ctypeRegex), val.Pattern...)
		return append(append(append(out, 0), val.Options...), 0)
	case *types.Document:
		out = append(out, ctypeObject)
		keys, values := val.Keys(), val.Values()
		order := make([]int, len(keys))
		for i := range order {
			order[i] = i
		}
		if canonical {
			slices.SortStableFunc(order, func(a, b int) int { return strings.Compare(keys[a], keys[b]) })
		}
		for _, i := range order {
			out = append(append(append(out, typeClass(values[i])), keys[i]...), 0)
			out = appendValue(out, values[i], canonical && keys[i] != "_id")
		}
		return append(out, 0)
	case *types.Array:
		out = append(out, ctypeArray)
		for i := range val.Len() {
			ev, _ := val.Get(i)
			out = appendValue(out, ev, canonical)
		}
		return append(out, 0)
	default:
		// No encoding: ValueLossy keeps such an index from serving queries.
		return append(out, ctypeNull)
	}
}

// typeClass is the byte an object field records for its value's type, ahead
// of the field name: one byte per comparison bracket, so every number and
// both booleans share one.
func typeClass(v any) byte {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return ctypeNumeric
	case bool:
		return ctypeBoolFalse
	}
	return appendValue(nil, v, false)[0]
}

// appendEscaped appends s with each 0x00 escaped as 0x00 0xFF, then a 0x00
// terminator.
func appendEscaped(out []byte, s string) []byte {
	for i := range len(s) {
		out = append(out, s[i])
		if s[i] == 0 {
			out = append(out, 0xFF)
		}
	}
	return append(out, 0)
}

func appendInt64(out []byte, n int64) []byte {
	switch {
	case n == 0:
		return append(out, ctypeZero)
	case n == math.MinInt64:
		// Its magnitude, shifted for the fraction flag, needs 65 bits.
		return appendFloat64(out, float64(n))
	case n < 0:
		return appendMagnitude(out, true, uint64(-n), 0, 0)
	}
	return appendMagnitude(out, false, uint64(n), 0, 0)
}

// appendMagnitude appends a number with integer part ip (1 <= ip < 2^63)
// and fraction frac bits left-aligned in fracBits bits (0 when integral).
// The integer part is stored shifted left one bit, its low bit flagging a
// fraction, in the fewest bytes; the fraction fills the rest of 8 bytes.
func appendMagnitude(out []byte, negative bool, ip, frac uint64, fracBits int) []byte {
	v := ip << 1
	if fracBits > 0 {
		v |= 1
	}
	n := byteLen(v)
	ctype := ctypePosInt1 + byte(n-1)
	if negative {
		ctype = ctypeNegInt1 - byte(n-1)
	}
	start := len(out) + 1
	out = append(out, ctype)
	for i := n - 1; i >= 0; i-- {
		out = append(out, byte(v>>(8*i)))
	}
	if fracBytes := 8 - n; fracBits > 0 && fracBytes > 0 {
		frac <<= 8*fracBytes - fracBits
		for i := fracBytes - 1; i >= 0; i-- {
			out = append(out, byte(frac>>(8*i)))
		}
	}
	if negative {
		complement(out[start:])
	}
	return out
}

func appendFloat64(out []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(out, ctypeNumeric)
	case f == 0:
		return append(out, ctypeZero)
	case math.IsInf(f, 1):
		return binary.BigEndian.AppendUint64(append(out, ctypePosLarge), math.MaxUint64)
	case math.IsInf(f, -1):
		return binary.BigEndian.AppendUint64(append(out, ctypeNegLarge), 0)
	}
	negative := f < 0
	mag := math.Abs(f)
	switch {
	case mag < 1:
		return appendSmall(out, negative, smallBits(mag))
	case mag < 1<<63:
		ip := math.Trunc(mag)
		fracBits := fractionBits(mag)
		frac := uint64((mag - ip) * math.Ldexp(1, fracBits))
		if frac == 0 {
			fracBits = 0
		}
		return appendMagnitude(out, negative, uint64(ip), frac, fracBits)
	}
	return appendLarge(out, negative, largeBits(mag))
}

// smallBits is the 8-byte encoding of mag in (0, 1): the IEEE bits shifted
// past the zero sign and top exponent bit, its two low bits clear, or for mag
// below 2^-255, the bits of mag scaled by 2^768 shifted one bit. The two meet
// at 2^-255.
func smallBits(mag float64) uint64 {
	if mag < 0x1p-255 {
		return math.Float64bits(mag*0x1p768) << 1
	}
	return math.Float64bits(mag) << 2
}

// largeBits is the 8-byte encoding of finite mag >= 2^63, its low bit clear.
func largeBits(mag float64) uint64 {
	return math.Float64bits(mag)<<1 - 1<<63
}

func appendSmall(out []byte, negative bool, enc uint64) []byte {
	if negative {
		return binary.BigEndian.AppendUint64(append(out, ctypeNegSmall), ^enc)
	}
	return binary.BigEndian.AppendUint64(append(out, ctypePosSmall), enc)
}

func appendLarge(out []byte, negative bool, enc uint64) []byte {
	if negative {
		return binary.BigEndian.AppendUint64(append(out, ctypeNegLarge), ^enc)
	}
	return binary.BigEndian.AppendUint64(append(out, ctypePosLarge), enc)
}

// fractionBits is the number of fraction bits a double of magnitude mag in
// [1, 2^63) carries below its binary point: 52 less its binary exponent, at
// least 0.
func fractionBits(mag float64) int {
	_, exp := math.Frexp(mag)
	return max(0, 53-exp)
}

func complement(b []byte) {
	for i := range b {
		b[i] = ^b[i]
	}
}

func byteLen(v uint64) int {
	n := 1
	for v > 0xFF {
		v >>= 8
		n++
	}
	return n
}

// EncodeValueLossy reports whether EncodeValue cannot represent v
// faithfully; an index containing any lossy entry must not serve queries.
func EncodeValueLossy(v any) bool {
	switch v.(type) {
	case nil, types.NullType, bool, int32, int64, float64, types.Decimal128, string, PreEncoded,
		types.ObjectID, time.Time, types.Binary, types.Timestamp,
		types.Regex, *types.Document, *types.Array,
		types.MaxKeyType, types.MinKeyType, types.UndefinedType:
		return false
	}
	return true
}

// ValueLossy reports whether v, or a value nested in it, cannot be encoded
// faithfully, so an index containing it must not be consulted.
func ValueLossy(v any) bool {
	switch v := v.(type) {
	case *types.Document:
		for _, fv := range v.Values() {
			if ValueLossy(fv) {
				return true
			}
		}
		return false
	case *types.Array:
		for i := range v.Len() {
			if ev, _ := v.Get(i); ValueLossy(ev) {
				return true
			}
		}
		return false
	}
	return EncodeValueLossy(v)
}

// SplitKeyValues splits key, the concatenated encodings of n index values, into
// each value's encoding. ok is false when key is not exactly n encodings.
func SplitKeyValues(key []byte, n int) (values [][]byte, ok bool) {
	for range n {
		l, ok := valueLen(key)
		if !ok {
			return nil, false
		}
		values = append(values, key[:l])
		key = key[l:]
	}
	return values, len(key) == 0
}

// valueLen returns the length of the value encoding that begins b.
func valueLen(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	fixed := func(l int) (int, bool) { return l, len(b) >= l }
	switch c := b[0]; {
	case c == ctypeMinKey, c == ctypeUndefined, c == ctypeNull, c == ctypeNumeric, c == ctypeZero,
		c == ctypeBoolFalse, c == ctypeBoolTrue, c == ctypeMaxKey:
		return 1, true
	case c >= ctypeNegLarge && c <= ctypePosLarge:
		return numberLen(b)
	case c == ctypeString:
		return escapedEnd(b, 1)
	case c == ctypeRegex:
		if l, ok := cstringEnd(b, 1); ok {
			return cstringEnd(b, l)
		}
	case c == ctypeOID:
		return fixed(13)
	case c == ctypeDate, c == ctypeTimestamp:
		return fixed(9)
	case c == ctypeBinData:
		if len(b) < 2 {
			return 0, false
		}
		if b[1] < 0xFF {
			return fixed(3 + int(b[1]))
		}
		if len(b) < 6 {
			return 0, false
		}
		return fixed(7 + int(binary.BigEndian.Uint32(b[2:6])))
	case c == ctypeObject:
		i := 1
		for i < len(b) && b[i] != 0 {
			l, ok := cstringEnd(b, i+1)
			if !ok {
				return 0, false
			}
			vl, ok := valueLen(b[l:])
			if !ok {
				return 0, false
			}
			i = l + vl
		}
		return i + 1, i < len(b)
	case c == ctypeArray:
		i := 1
		for i < len(b) && b[i] != 0 {
			vl, ok := valueLen(b[i:])
			if !ok {
				return 0, false
			}
			i += vl
		}
		return i + 1, i < len(b)
	}
	return 0, false
}

// numberLen returns the length of the number encoding that begins b.
func numberLen(b []byte) (int, bool) {
	c := b[0]
	negative := c < ctypeZero
	word := func(at int) (uint64, bool) {
		if len(b) < at+8 {
			return 0, false
		}
		w := binary.BigEndian.Uint64(b[at:])
		if negative {
			w = ^w
		}
		return w, true
	}
	switch c {
	case ctypePosSmall, ctypeNegSmall:
		w, ok := word(1)
		if !ok {
			return 0, false
		}
		if w < smallestDoubleBits || w&3 == decimalBelow || w&3 == decimalAbove {
			return 17, len(b) >= 17
		}
		return 9, true
	case ctypePosLarge, ctypeNegLarge:
		w, ok := word(1)
		if !ok {
			return 0, false
		}
		if w != math.MaxUint64 && (w > largestDoubleBits || w&1 == 1) {
			return 17, len(b) >= 17
		}
		return 9, true
	}
	n := int(c-ctypePosInt1) + 1
	if negative {
		n = int(ctypeNegInt1-c) + 1
	}
	if len(b) < 1+n {
		return 0, false
	}
	low := b[n]
	if negative {
		low = ^low
	}
	if low&1 == 0 {
		return 1 + n, true
	}
	if n == 8 {
		// No fraction bytes fit; a decimal fraction follows as a continuation.
		return 17, len(b) >= 17
	}
	if len(b) < 9 {
		return 0, false
	}
	last := b[8]
	if negative {
		last = ^last
	}
	if last&3 == decimalBelow || last&3 == decimalAbove {
		return 17, len(b) >= 17
	}
	return 9, true
}

func escapedEnd(b []byte, from int) (int, bool) {
	for i := from; i < len(b); i++ {
		if b[i] == 0 {
			if i+1 < len(b) && b[i+1] == 0xFF {
				i++
				continue
			}
			return i + 1, true
		}
	}
	return 0, false
}

func cstringEnd(b []byte, from int) (int, bool) {
	for i := from; i < len(b); i++ {
		if b[i] == 0 {
			return i + 1, true
		}
	}
	return 0, false
}

// BracketRange returns the [start, stop) KeyString range of v's
// MongoDB type bracket, used to clamp one-sided comparisons (operators
// never match across brackets). The numeric bracket excludes NaN but
// includes the infinities. ok=false when comparison semantics do not
// reduce to a contiguous range.
func BracketRange(v any) (start, stop []byte, ok bool) {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return []byte{ctypeNegLarge}, []byte{ctypePosLarge + 1}, true
	case string, PreEncoded:
		return []byte{ctypeString}, []byte{ctypeString + 1}, true
	case time.Time:
		return []byte{ctypeDate}, []byte{ctypeDate + 1}, true
	case types.ObjectID:
		return []byte{ctypeOID}, []byte{ctypeOID + 1}, true
	case bool:
		return []byte{ctypeBoolFalse}, []byte{ctypeBoolTrue + 1}, true
	case types.Binary:
		return []byte{ctypeBinData}, []byte{ctypeBinData + 1}, true
	case types.Timestamp:
		return []byte{ctypeTimestamp}, []byte{ctypeTimestamp + 1}, true
	}
	return nil, nil, false
}
