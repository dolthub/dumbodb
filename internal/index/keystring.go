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
// and relative sort position:
//
//	Null/Missing = 0x14 (20)
//	Numbers      = 0x1E–0x33 (30–51)
//	String       = 0x3C (60)
//	Object       = 0x46 (70)
//	Array        = 0x50 (80)
//	BinData      = 0x5A (90)
//	OID          = 0x64 (100)
//	Bool false   = 0x6E (110)
//	Bool true    = 0x6F (111)
//	Date         = 0x78 (120)
//	MaxKey       = 0xF0 (240)
//
// Numeric encoding uses magnitude bucketing so that numbers of the same value
// (regardless of int32/int64/double type) encode identically.
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
	ctypeNull      = byte(0x14) // Null / missing field
	ctypeNegLarge  = byte(0x1E) // Negative, magnitude ≥ 2^56
	ctypeNegMedium = byte(0x22) // Negative, smaller magnitudes (see numeric encoding)
	ctypeZero      = byte(0x29) // Numeric zero
	ctypePosMedium = byte(0x2B) // Positive, smaller magnitudes
	ctypePosLarge  = byte(0x33) // Positive, magnitude ≥ 2^56
	ctypeString    = byte(0x3C) // UTF-8 string
	ctypeObject    = byte(0x46) // Embedded document
	ctypeArray     = byte(0x50) // Array
	ctypeBinData   = byte(0x5A) // Binary data
	ctypeOID       = byte(0x64) // ObjectID
	ctypeBoolFalse = byte(0x6E) // Boolean false
	ctypeBoolTrue  = byte(0x6F) // Boolean true
	ctypeDate      = byte(0x78) // UTC datetime
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

	case *types.Document:
		// Object: minimal encoding for PoC — just mark type, no nested sort.
		return []byte{ctypeObject}

	case *types.Array:
		return []byte{ctypeArray}

	case types.MaxKeyType:
		return []byte{ctypeMaxKey}

	case types.MinKeyType:
		// MinKey sorts before everything including Null.
		return []byte{0x10}

	default:
		// Unknown type: encode as null so it at least doesn't panic.
		return []byte{ctypeNull}
	}
}

// encodeString encodes a UTF-8 string with 0x00 bytes escaped as 0x00 0xFF.
// Format: [0x3C][bytes, 0x00→0x00 0xFF][0x00 terminator]
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
// We encode abs(n)-1 (so -1 → 0, -256 → 255, etc.), then bit-flip all value bytes.
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
	// Bit-flipping achieves this: larger mag → larger bytes before flip → smaller after flip.
	for i := byteCount - 1; i >= 0; i-- {
		out[1+i] = ^byte(mag)
		mag >>= 8
	}
	return out
}

// encodeFloat64 encodes a float64. NaN and ±Inf are treated as null.
func encodeFloat64(f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte{ctypeNull}
	}
	if f == 0 {
		return []byte{ctypeZero}
	}
	// Check if it's an integer value we can encode exactly.
	if f == math.Trunc(f) && f >= math.MinInt64 && f <= math.MaxInt64 {
		return encodeInt64(int64(f))
	}
	// Non-integer float: encode using IEEE 754 bit pattern with sign adjustment
	// so that byte order = numeric order.
	bits := math.Float64bits(f)
	// For positive floats: set MSB to ensure they sort after integers.
	// For negative floats: flip all bits so negatives sort below positives.
	var out [9]byte
	if f > 0 {
		// Positive non-integer: encode in the positive range.
		// Use ctypePosMedium+7 (largest positive bucket) + bit-adjusted bytes.
		bits |= 1 << 63
		out[0] = ctypePosLarge
		binary.BigEndian.PutUint64(out[1:], bits)
	} else {
		// Negative non-integer.
		bits = ^bits
		out[0] = ctypeNegLarge
		binary.BigEndian.PutUint64(out[1:], bits)
	}
	return out[:]
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
