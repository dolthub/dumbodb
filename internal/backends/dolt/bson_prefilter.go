// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package dolt

import (
	"bytes"
	"encoding/binary"
	"math"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// BSON-element prefilter for the bson-a storage format. Replaces the
// ExtJSON byte-substring matcher with a structurally aware walker
// that decodes top-level BSON elements directly.
//
// The prefilter is a SOUND optimisation: when it returns false the
// document is guaranteed not to match, so the iterator can skip it
// without decoding. False positives (predicate returns true for a
// doc that doesn't actually match) are handled downstream by the
// handler's FilterIterator.
//
// All predicates operate on bson-a stored bytes (1-byte version
// header followed by raw BSON). The version byte is stripped before
// walking; a doc with an unknown version byte is treated permissively
// (predicate returns true) so the iterator falls back to the
// canonical decode + filter path.

// BSON element type bytes used by this file. Duplicated from the
// bsonindexed package to keep the prefilter in-package and avoid the
// extra import-time coupling.
const (
	bsonTypeDouble   byte = 0x01
	bsonTypeString   byte = 0x02
	bsonTypeDocument byte = 0x03
	bsonTypeArray    byte = 0x04
	bsonTypeBinary   byte = 0x05
	bsonTypeObjectID byte = 0x07
	bsonTypeBool     byte = 0x08
	bsonTypeDate     byte = 0x09
	bsonTypeNull     byte = 0x0A
	bsonTypeRegex    byte = 0x0B
	bsonTypeInt32    byte = 0x10
	bsonTypeTime     byte = 0x11
	bsonTypeInt64    byte = 0x12
	bsonTypeDecimal  byte = 0x13
	bsonTypeMinKey   byte = 0xFF
	bsonTypeMaxKey   byte = 0x7F
)

// buildBSONFieldPredicate replaces the historical extJSONFieldPatterns
// path. Returns nil when no sound predicate can be expressed for the
// (field, value) clause; the iterator falls back to a full scan in
// that case.
//
// Supported filter shapes:
//
//   - Scalar equality on int32 / int64 / float64 (cross-type numeric
//     equivalence per mongo's number-coercion rules).
//   - Scalar equality on string, bool, time.Time, types.ObjectID.
//   - Numeric range operators {$gt, $gte, $lt, $lte} via the dedicated
//     range predicate.
//   - Top-level array containment: {field: scalar} matches a stored
//     array whose elements include the scalar.
//
// Unsupported (returns nil):
//
//   - Dotted-path keys (the prefilter only inspects the top level).
//   - Operator docs other than the numeric range set.
//   - Regex, $in, $nin, $ne, and other operator filters.
//   - NaN / +-Inf doubles.
//   - decimal128 (kept simple; the handler can re-check).
func buildBSONFieldPredicate(field string, value any) func([]byte) bool {
	if opDoc, ok := value.(*types.Document); ok {
		return buildBSONNumericRangePredicate(field, opDoc)
	}
	switch v := value.(type) {
	case *types.Array, types.NullType, types.Regex, types.Decimal128:
		_ = v
		return nil
	}
	target := normaliseFilterValue(value)
	if target == nil {
		return nil
	}
	fieldBytes := []byte(field)
	return func(storedBytes []byte) bool {
		bsonBytes, ok := stripVersionPermissive(storedBytes)
		if !ok {
			return true
		}
		return walkTopLevelEquality(bsonBytes, fieldBytes, target)
	}
}

// buildBSONNumericRangePredicate is the BSON analogue of the JSON
// scanTopLevelNumericExtJSON walker. Compiles {$gt, $gte, $lt, $lte}
// into running bounds and returns a predicate that walks the stored
// BSON, locates the top-level numeric field, and tests inclusion.
// Returns nil for operator docs that mix in unsupported operators
// or non-numeric bounds.
func buildBSONNumericRangePredicate(field string, opDoc *types.Document) func([]byte) bool {
	keys := opDoc.Keys()
	if len(keys) == 0 {
		return nil
	}
	var (
		hasLo, loIncl bool
		lo            float64
		hasHi, hiIncl bool
		hi            float64
	)
	for _, k := range keys {
		ov, err := opDoc.Get(k)
		if err != nil {
			return nil
		}
		fv, ok := numericBoundFloat64(ov)
		if !ok {
			return nil
		}
		switch k {
		case "$gt":
			lo, loIncl, hasLo = tightenLo(hasLo, lo, loIncl, fv, false)
		case "$gte":
			lo, loIncl, hasLo = tightenLo(hasLo, lo, loIncl, fv, true)
		case "$lt":
			hi, hiIncl, hasHi = tightenHi(hasHi, hi, hiIncl, fv, false)
		case "$lte":
			hi, hiIncl, hasHi = tightenHi(hasHi, hi, hiIncl, fv, true)
		default:
			return nil
		}
	}
	if !hasLo && !hasHi {
		return nil
	}
	fieldBytes := []byte(field)
	return func(storedBytes []byte) bool {
		bsonBytes, ok := stripVersionPermissive(storedBytes)
		if !ok {
			return true
		}
		v, status := scanTopLevelBSONNumeric(bsonBytes, fieldBytes)
		switch status {
		case rangeProbeBail:
			return true
		case rangeProbeMissing:
			return false
		}
		if hasLo {
			if loIncl {
				if !(v >= lo) {
					return false
				}
			} else if !(v > lo) {
				return false
			}
		}
		if hasHi {
			if hiIncl {
				if !(v <= hi) {
					return false
				}
			} else if !(v < hi) {
				return false
			}
		}
		return true
	}
}

// stripVersionPermissive strips the 1-byte format version. Returns
// ok=false when the version byte is unrecognised; callers treat that
// as permissive (predicate returns true). Distinct from
// bson_codec.stripVersion which errors out -- the prefilter never
// errors.
func stripVersionPermissive(stored []byte) ([]byte, bool) {
	if len(stored) < 1 || stored[0] != bsonFormatVersion {
		return nil, false
	}
	return stored[1:], true
}

// normaliseFilterValue converts a filter-side value into a stable
// shape that the equality walker can compare against. Returns nil
// for unsupported types so callers can drop the prefilter cleanly.
// time.Time is returned as-is to preserve the date-vs-int64 type
// distinction the BSON walker needs; ObjectID is returned as a
// [12]byte for direct byte comparison.
func normaliseFilterValue(v any) any {
	switch t := v.(type) {
	case int32, int64, float64:
		return t
	case string:
		return t
	case bool:
		return t
	case time.Time:
		return t
	case types.ObjectID:
		return [12]byte(t)
	}
	return nil
}

// walkTopLevelEquality scans the top-level fields of doc. When it
// encounters the named field, it compares the stored value against
// target. For array-typed stored values it additionally tests every
// element so {tags: "go"} matches a stored ["go", "rust"]. Returns
// true on match; false on confirmed mismatch; true (permissive) on
// any structural anomaly (under-sized buffer, unknown type byte).
func walkTopLevelEquality(doc []byte, field []byte, target any) bool {
	if len(doc) < 5 {
		return true
	}
	docLen := int(binary.LittleEndian.Uint32(doc))
	if docLen > len(doc) {
		return true
	}
	end := docLen - 1
	pos := 4
	for pos < end {
		typeByte := doc[pos]
		if typeByte == 0x00 {
			break
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && doc[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			return true
		}
		valueStart := nameEnd + 1
		valueEnd, ok := bsonValueEnd(doc, valueStart, typeByte, end)
		if !ok {
			return true
		}
		if bytes.Equal(doc[nameStart:nameEnd], field) {
			return bsonValueEqualsOrContains(typeByte, doc[valueStart:valueEnd], target)
		}
		pos = valueEnd
	}
	return false
}

// bsonValueEqualsOrContains reports whether the BSON value at
// (typeByte, valueBytes) equals target. For container types it
// recursively walks elements: array elements are compared (mongo
// array-contains semantics for {field: scalar}); documents are not
// considered as "containing" scalars (the prefilter doesn't model
// sub-document equality).
func bsonValueEqualsOrContains(typeByte byte, valueBytes []byte, target any) bool {
	if scalarMatch(typeByte, valueBytes, target) {
		return true
	}
	if typeByte != bsonTypeArray {
		return false
	}
	if len(valueBytes) < 5 {
		return true
	}
	arrLen := int(binary.LittleEndian.Uint32(valueBytes))
	if arrLen > len(valueBytes) {
		return true
	}
	arrEnd := arrLen - 1
	pos := 4
	for pos < arrEnd {
		tb := valueBytes[pos]
		if tb == 0x00 {
			break
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < arrEnd && valueBytes[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= arrEnd {
			return true
		}
		vStart := nameEnd + 1
		vEnd, ok := bsonValueEnd(valueBytes, vStart, tb, arrEnd)
		if !ok {
			return true
		}
		if scalarMatch(tb, valueBytes[vStart:vEnd], target) {
			return true
		}
		pos = vEnd
	}
	return false
}

// scalarMatch reports whether the BSON-typed value bytes equal the
// normalised target. Numeric equality coerces across int32/int64/
// double per mongo semantics; string / bool / date / objectid match
// on their native shape only.
func scalarMatch(typeByte byte, valueBytes []byte, target any) bool {
	switch t := target.(type) {
	case int32:
		f, ok := bsonNumericAsFloat(typeByte, valueBytes)
		return ok && f == float64(t)
	case int64:
		f, ok := bsonNumericAsFloat(typeByte, valueBytes)
		return ok && f == float64(t)
	case float64:
		f, ok := bsonNumericAsFloat(typeByte, valueBytes)
		if !ok {
			return false
		}
		// Exact float compare; NaN never matches.
		if math.IsNaN(t) || math.IsNaN(f) {
			return false
		}
		return f == t
	case string:
		if typeByte != bsonTypeString {
			return false
		}
		if len(valueBytes) < 4 {
			return false
		}
		strLen := int(binary.LittleEndian.Uint32(valueBytes))
		if strLen < 1 || 4+strLen > len(valueBytes) {
			return false
		}
		return string(valueBytes[4:4+strLen-1]) == t
	case bool:
		if typeByte != bsonTypeBool || len(valueBytes) < 1 {
			return false
		}
		return (valueBytes[0] != 0) == t
	case [12]byte:
		if typeByte != bsonTypeObjectID || len(valueBytes) < 12 {
			return false
		}
		return bytes.Equal(valueBytes[:12], t[:])
	case time.Time:
		if typeByte != bsonTypeDate || len(valueBytes) < 8 {
			return false
		}
		stored := int64(binary.LittleEndian.Uint64(valueBytes))
		return stored == t.UnixMilli()
	}
	return false
}

// bsonNumericAsFloat decodes a numeric BSON value to float64. Returns
// ok=false for non-numeric types or numerics outside the
// float64-exact range (large int64 that would lose precision is
// reported as ok=false so the predicate falls back to permissive).
func bsonNumericAsFloat(typeByte byte, valueBytes []byte) (float64, bool) {
	switch typeByte {
	case bsonTypeInt32:
		if len(valueBytes) < 4 {
			return 0, false
		}
		return float64(int32(binary.LittleEndian.Uint32(valueBytes))), true
	case bsonTypeInt64:
		if len(valueBytes) < 8 {
			return 0, false
		}
		n := int64(binary.LittleEndian.Uint64(valueBytes))
		f := float64(n)
		if int64(f) != n {
			return 0, false
		}
		return f, true
	case bsonTypeDouble:
		if len(valueBytes) < 8 {
			return 0, false
		}
		bits := binary.LittleEndian.Uint64(valueBytes)
		return math.Float64frombits(bits), true
	}
	return 0, false
}

// bsonValueEnd returns the byte offset just past the end of the BSON
// value starting at valueStart, given its type byte. Mirrors
// bsonindexed.elementValueEnd; duplicated here to avoid the cross-
// package import for one helper.
func bsonValueEnd(buf []byte, valueStart int, typeByte byte, hardEnd int) (int, bool) {
	switch typeByte {
	case bsonTypeDouble, bsonTypeDate, bsonTypeTime, bsonTypeInt64:
		end := valueStart + 8
		return end, end <= hardEnd+1
	case bsonTypeInt32:
		end := valueStart + 4
		return end, end <= hardEnd+1
	case bsonTypeBool:
		end := valueStart + 1
		return end, end <= hardEnd+1
	case bsonTypeObjectID:
		end := valueStart + 12
		return end, end <= hardEnd+1
	case bsonTypeNull, bsonTypeMinKey, bsonTypeMaxKey:
		return valueStart, true
	case bsonTypeString:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		strLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + 4 + strLen
		return end, end <= hardEnd+1
	case bsonTypeBinary:
		if valueStart+5 > len(buf) {
			return 0, false
		}
		binLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + 5 + binLen
		return end, end <= hardEnd+1
	case bsonTypeDecimal:
		end := valueStart + 16
		return end, end <= hardEnd+1
	case bsonTypeRegex:
		end := valueStart
		for k := 0; k < 2; k++ {
			for end < len(buf) && buf[end] != 0x00 {
				end++
			}
			if end >= len(buf) {
				return 0, false
			}
			end++
		}
		return end, end <= hardEnd+1
	case bsonTypeDocument, bsonTypeArray:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		containerLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + containerLen
		return end, end <= hardEnd+1
	}
	return 0, false
}

// scanTopLevelBSONNumeric is the BSON analogue of the JSON walker:
// finds a top-level numeric field by name and returns its float64
// value. The status return distinguishes a found numeric, a missing
// field (range can prove no match), and a structural anomaly that
// forces the predicate to be permissive.
func scanTopLevelBSONNumeric(doc []byte, field []byte) (float64, rangeProbeStatus) {
	if len(doc) < 5 {
		return 0, rangeProbeBail
	}
	docLen := int(binary.LittleEndian.Uint32(doc))
	if docLen > len(doc) {
		return 0, rangeProbeBail
	}
	end := docLen - 1
	pos := 4
	for pos < end {
		typeByte := doc[pos]
		if typeByte == 0x00 {
			return 0, rangeProbeMissing
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && doc[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			return 0, rangeProbeBail
		}
		valueStart := nameEnd + 1
		valueEnd, ok := bsonValueEnd(doc, valueStart, typeByte, end)
		if !ok {
			return 0, rangeProbeBail
		}
		if bytes.Equal(doc[nameStart:nameEnd], field) {
			f, ok := bsonNumericAsFloat(typeByte, doc[valueStart:valueEnd])
			if !ok {
				return 0, rangeProbeBail
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, rangeProbeBail
			}
			return f, rangeProbeFound
		}
		pos = valueEnd
	}
	return 0, rangeProbeMissing
}

// numericBoundFloat64 mirrors the JSON-side numericBoundToFloat64.
// Converts a bound value to float64 only when the conversion is
// exact and finite.
func numericBoundFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int32:
		return float64(n), true
	case int64:
		f := float64(n)
		if int64(f) != n {
			return 0, false
		}
		return f, true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// tightenLo / tightenHi merge a new bound into the running tightest
// bound. Mirrors the JSON-side helpers.
func tightenLo(curHas bool, curVal float64, curIncl bool, nv float64, nIncl bool) (float64, bool, bool) {
	if !curHas || nv > curVal {
		return nv, nIncl, true
	}
	if nv < curVal {
		return curVal, curIncl, true
	}
	return curVal, curIncl && nIncl, true
}

func tightenHi(curHas bool, curVal float64, curIncl bool, nv float64, nIncl bool) (float64, bool, bool) {
	if !curHas || nv < curVal {
		return nv, nIncl, true
	}
	if nv > curVal {
		return curVal, curIncl, true
	}
	return curVal, curIncl && nIncl, true
}
