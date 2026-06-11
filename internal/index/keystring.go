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
// KeyString is a byte-comparable encoding of BSON values inspired by MongoDB's
// internal KeyString format. It guarantees that the encoded bytes sort in the
// same order as the original values, enabling direct byte-comparison in prolly
// trees.
//
// Each encoded value starts with a CType byte that determines the value's type
// and relative sort position, matching MongoDB's documented BSON
// type-comparison order:
//
//	MinKey       = 0x10 (16)
//	Null/Missing = 0x14 (20)
//	NaN          = 0x19 (25)  -- lowest of the numerics
//	-Infinity    = 0x1A (26)
//	Negatives    = 0x1B --0x22 (27 --34)
//	Zero         = 0x29 (41)
//	Positives    = 0x2B --0x32 (43 --50)
//	+Infinity    = 0x33 (51)
//	String       = 0x3C (60)
//	Object       = 0x46 (70)
//	Array        = 0x50 (80)
//	BinData      = 0x5A (90)
//	OID          = 0x64 (100)
//	Bool false   = 0x6E (110)
//	Bool true    = 0x6F (111)
//	Date         = 0x78 (120)
//	Timestamp    = 0x82 (130)
//	Regex        = 0x8C (140)
//	MaxKey       = 0xF0 (240)
//
// Numeric encoding uses magnitude bucketing on the integer part so that
// numbers of the same value (regardless of int32/int64/double type) encode
// identically, with an 8-byte fraction continuation for non-integer doubles
// so mixed int/double data sorts by numeric value.
//
// String encoding: [0x3C][UTF-8 bytes with 0x00 escaped as 0x00 0xFF][0x00 terminator]
package index

import (
	"encoding/binary"
	"math"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// CType constants for the first byte of a KeyString-encoded value.
const (
	ctypeMinKey    = byte(0x10) // MinKey
	ctypeNull      = byte(0x14) // Null / missing field
	ctypeNaN       = byte(0x19) // NaN: sorts below every other number
	ctypeNegInf    = byte(0x1A) // -Infinity
	ctypeNegMedium = byte(0x22) // Negative integer-part buckets: 0x22 down to 0x1B
	ctypeZero      = byte(0x29) // Numeric zero
	ctypePosMedium = byte(0x2B) // Positive integer-part buckets: 0x2B up to 0x32
	ctypePosInf    = byte(0x33) // +Infinity: sorts above every other number
	ctypeString    = byte(0x3C) // UTF-8 string
	ctypeObject    = byte(0x46) // Embedded document
	ctypeArray     = byte(0x50) // Array
	ctypeBinData   = byte(0x5A) // Binary data
	ctypeOID       = byte(0x64) // ObjectID
	ctypeBoolFalse = byte(0x6E) // Boolean false
	ctypeBoolTrue  = byte(0x6F) // Boolean true
	ctypeDate      = byte(0x78) // UTC datetime
	ctypeTimestamp = byte(0x82) // BSON Timestamp (internal replication type)
	ctypeRegex     = byte(0x8C) // Regular expression (pattern + options)
	ctypeMaxKey    = byte(0xF0) // MaxKey
)

// EncodeValue encodes a single BSON value to its KeyString bytes.
// The result is byte-comparable and preserves the sort order of the original value.
func EncodeValue(v any) []byte {
	switch val := v.(type) {
	case nil, types.NullType:
		return []byte{ctypeNull}

	case bool:
		if val {
			return []byte{ctypeBoolTrue}
		}
		return []byte{ctypeBoolFalse}

	case int32:
		return encodeInt64(int64(val))

	case int64:
		return encodeInt64(val)

	case float64:
		return encodeFloat64(val)

	case string:
		return encodeString(val)

	case types.ObjectID:
		b := make([]byte, 13)
		b[0] = ctypeOID
		copy(b[1:], val[:])
		return b

	case time.Time:
		b := make([]byte, 9)
		b[0] = ctypeDate
		// Big-endian so byte order = time order.
		binary.BigEndian.PutUint64(b[1:], uint64(val.UnixMilli()))
		return b

	case types.Binary:
		// [ctype][subtype][data]
		b := make([]byte, 2+len(val.B))
		b[0] = ctypeBinData
		b[1] = byte(val.Subtype)
		copy(b[2:], val.B)
		return b

	case types.Timestamp:
		// Timestamp is (T<<32|I); uint64 order is its sort order.
		b := make([]byte, 9)
		b[0] = ctypeTimestamp
		binary.BigEndian.PutUint64(b[1:], uint64(val))
		return b

	case types.Regex:
		// [ctype][pattern, 0x00-escaped][0x00][options, 0x00-escaped][0x00].
		out := []byte{ctypeRegex}
		out = appendEscaped(out, val.Pattern)
		out = appendEscaped(out, val.Options)
		return out

	case *types.Document:
		// Bracket marker only, no nested sort: sound because the
		// planner rejects object operands (queries never reach here).
		return []byte{ctypeObject}

	case *types.Array:
		return []byte{ctypeArray}

	case types.MaxKeyType:
		return []byte{ctypeMaxKey}

	case types.MinKeyType:
		// MinKey sorts before everything including Null.
		return []byte{ctypeMinKey}

	default:
		// No faithful encoding (Decimal128, future types): the index is
		// flagged lossy (EncodeValueLossy) and never consulted.
		return []byte{ctypeNull}
	}
}

// EncodeValueLossy reports whether EncodeValue cannot represent v
// faithfully; an index containing any lossy entry must not serve
// queries. Document/Array markers collapse only within their own
// bracket and their operands are rejected by the planner, so they are
// not lossy in this sense.
func EncodeValueLossy(v any) bool {
	switch v.(type) {
	case nil, types.NullType, bool, int32, int64, float64, string,
		types.ObjectID, time.Time, types.Binary, types.Timestamp,
		types.Regex, *types.Document, *types.Array,
		types.MaxKeyType, types.MinKeyType:
		return false
	}
	// Decimal128 and anything else without a case in EncodeValue.
	return true
}

// ValueLossy reports whether v cannot be faithfully represented by the
// KeyString encoding, so an index containing it must not be consulted by the
// planner. This is EncodeValueLossy plus a value-aware check for
// large-magnitude integral float64s: encodeFloat64 takes the integer part
// through a uint64/int64 conversion, and Go's float-to-integer conversion is
// implementation-defined when the value is out of range. Such doubles would
// otherwise corrupt index ordering and unique-key semantics.
func ValueLossy(v any) bool {
	if EncodeValueLossy(v) {
		return true
	}

	f, ok := v.(float64)
	if !ok {
		return false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f == 0 {
		return false
	}
	// Non-integral doubles have |f| < 2^52 and encode safely. Integral doubles
	// in [MinInt64, MaxUint64] also encode without overflow; beyond that the
	// integer-part conversion in encodeFloat64 overflows.
	const (
		maxEncodable = float64(1<<63) * 2 // 2^64
		minEncodable = float64(-(1 << 63)) // math.MinInt64
	)
	return f == math.Trunc(f) && (f >= maxEncodable || f < minEncodable)
}

func appendEscaped(out []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		b := s[i]
		out = append(out, b)
		if b == 0x00 {
			out = append(out, 0xFF)
		}
	}
	return append(out, 0x00)
}

// encodeString encodes a UTF-8 string with 0x00 bytes escaped as 0x00 0xFF.
// Format: [0x3C][bytes, 0x00->0x00 0xFF][0x00 terminator]
func encodeString(s string) []byte {
	raw := []byte(s)
	// Pre-calculate output size (each 0x00 becomes 2 bytes).
	size := 2 // ctype + terminator
	for _, b := range raw {
		if b == 0x00 {
			size++
		}
		size++
	}

	out := make([]byte, 0, size)
	out = append(out, ctypeString)
	for _, b := range raw {
		out = append(out, b)
		if b == 0x00 {
			out = append(out, 0xFF) // escape
		}
	}
	out = append(out, 0x00) // terminator
	return out
}

// encodeInt64 encodes an integer using magnitude bucketing so that int32 and
// int64 values of equal magnitude produce identical bytes. Magnitude bucketing
// also ensures the byte comparison order matches the numeric order.
//
// Encoding:
//   - Zero:  [ctypeZero]
//   - Positive: [ctypePos1..ctypePos8][big-endian unsigned bytes, zero-padded left]
//   - Negative: [ctypeNeg8..ctypeNeg1][big-endian bitflipped bytes]
//
// Positive values sort by magnitude byte (pos small < pos large), then by value.
// Negative values sort by magnitude byte (neg large < neg small), then by inverted value.
func encodeInt64(n int64) []byte {
	if n == 0 {
		return []byte{ctypeZero}
	}
	if n > 0 {
		return encodePosInt(uint64(n))
	}
	return encodeNegInt(n)
}

// encodePosInt encodes a positive integer.
// Uses 1-8 bytes to store the value, big-endian, with the smallest necessary byte count.
// CType = ctypePosMedium + (byteCount-1), so 1-byte ints get ctypePosMedium,
// 8-byte ints get ctypePosMedium+7 = 0x32.
func encodePosInt(n uint64) []byte {
	byteCount := minBytesForUint(n)
	out := make([]byte, 1+byteCount)
	out[0] = ctypePosMedium + byte(byteCount-1)
	// Write big-endian, right-aligned.
	for i := byteCount - 1; i >= 0; i-- {
		out[1+i] = byte(n)
		n >>= 8
	}
	return out
}

// encodeNegInt encodes a negative integer.
// We encode abs(n)-1 (so -1 -> 0, -256 -> 255, etc.), then bit-flip all value bytes.
// The CType encodes magnitude so larger magnitudes sort first (more negative = smaller).
// CType = ctypeNegMedium - (byteCount-1) ensures descending ctype order.
// Actually: for negative numbers, ctype = ctypeNegMedium + (8 - byteCount) and
// we want more-negative (larger magnitude) to sort first.
//
// Simpler approach: negate to positive, encode as positive with ctypeNeg range,
// and invert the value bytes so that larger negatives have lower byte values.
func encodeNegInt(n int64) []byte {
	// Convert to positive magnitude for encoding.
	// n is negative, so abs = -n. But -math.MinInt64 overflows, handle that edge case.
	var mag uint64
	if n == math.MinInt64 {
		mag = uint64(math.MaxInt64) + 1
	} else {
		mag = uint64(-n)
	}

	byteCount := minBytesForUint(mag)
	// Negate ctype: neg numbers use ctypeNegMedium down to ctypeNegMedium-7.
	// We want the ctype to decrease as magnitude increases, so larger-magnitude
	// negatives sort before smaller-magnitude ones.
	// ctypeNegMedium = 0x22; we subtract (byteCount - 1) from it.
	ctype := ctypeNegMedium - byte(byteCount-1)

	out := make([]byte, 1+byteCount)
	out[0] = ctype
	// Write big-endian magnitude, then bit-flip the value bytes so that
	// more-negative values (larger mag) sort before less-negative (smaller mag)
	// within the same ctype bucket.
	// Since ctype already handles cross-magnitude ordering, within same ctype
	// we need the bytes to sort in value order (more negative = smaller).
	// Bit-flipping achieves this: larger mag -> larger bytes before flip -> smaller after flip.
	for i := byteCount - 1; i >= 0; i-- {
		out[1+i] = ^byte(mag)
		mag >>= 8
	}
	return out
}

// encodeFloat64: NaN/infinities get sentinels at the numeric bracket
// edges (MongoDB sort order); integral doubles reuse the integer
// encoding so 2 == 2.0 == int32(2). A non-integer double encodes as
// its integer part plus an 8-byte fraction continuation -- the
// integer-only encoding is a strict prefix, so 2 < 2.5 < 3 bytewise.
// Negative non-integers encode relative to ceil(-x): x = -m + g,
// g in (0,1), so -3 < -2.9 < -2.5 < -2 bytewise.
func encodeFloat64(f float64) []byte {
	if math.IsNaN(f) {
		return []byte{ctypeNaN}
	}
	if math.IsInf(f, -1) {
		return []byte{ctypeNegInf}
	}
	if math.IsInf(f, 1) {
		return []byte{ctypePosInf}
	}
	if f == 0 {
		return []byte{ctypeZero}
	}
	// Integral doubles share the integer encoding exactly.
	if f == math.Trunc(f) && f >= math.MinInt64 && f <= math.MaxInt64 {
		return encodeInt64(int64(f))
	}

	if f > 0 {
		// Doubles >= 2^52 are integral, so the integer part fits uint64.
		ip := math.Floor(f)
		out := encodePosInt(uint64(ip))
		return appendFraction(out, f-ip)
	}

	mag := math.Ceil(-f)
	out := encodeNegInt(int64(-mag))
	return appendFraction(out, mag+f)
}

// appendFraction appends the 8-byte big-endian IEEE 754 bit pattern of
// frac in (0, 1): monotone with the value, collision-free.
func appendFraction(out []byte, frac float64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(frac))
	return append(out, b[:]...)
}

// BracketRange returns the [start, stop) KeyString range of v's
// MongoDB type bracket, used to clamp one-sided comparisons (operators
// never match across brackets). The numeric bracket excludes NaN but
// includes the infinities. ok=false when comparison semantics do not
// reduce to a contiguous range.
func BracketRange(v any) (start, stop []byte, ok bool) {
	switch v.(type) {
	case int32, int64, float64:
		// [-Inf .. +Inf], excluding NaN below it.
		return []byte{ctypeNegInf}, []byte{ctypePosInf + 1}, true
	case string:
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

// minBytesForUint returns the minimum number of bytes needed to represent n.
func minBytesForUint(n uint64) int {
	switch {
	case n <= 0xFF:
		return 1
	case n <= 0xFFFF:
		return 2
	case n <= 0xFFFFFF:
		return 3
	case n <= 0xFFFFFFFF:
		return 4
	case n <= 0xFFFFFFFFFF:
		return 5
	case n <= 0xFFFFFFFFFFFF:
		return 6
	case n <= 0xFFFFFFFFFFFFFF:
		return 7
	default:
		return 8
	}
}
