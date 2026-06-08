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

// Sound prefilter on bson-a stored bytes: when a predicate returns false
// the document is guaranteed not to match. False positives are re-checked
// downstream by the handler's FilterIterator. Unknown version bytes are
// treated permissively so the iterator falls back to canonical decode.

package dolt

import (
	"bytes"
	"encoding/binary"
	"math"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

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

// buildBSONFieldPredicate returns a sound predicate for (field, value),
// or nil when no sound predicate can be expressed (dotted paths, regex,
// $in/$nin/$ne, NaN/Inf, decimal128, ...). Supported: scalar equality on
// numeric/string/bool/time/ObjectID, numeric range via $gt/$gte/$lt/$lte,
// and top-level array containment of a scalar.
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

// stripVersionPermissive returns ok=false on unrecognised version bytes;
// callers treat that as permissive.
func stripVersionPermissive(stored []byte) ([]byte, bool) {
	if len(stored) < 1 || stored[0] != bsonFormatVersion {
		return nil, false
	}
	return stored[1:], true
}

// normaliseFilterValue returns nil for types the equality walker does
// not compare. ObjectID is returned as [12]byte for direct byte compare.
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

// walkTopLevelEquality returns true on match, false on confirmed
// mismatch, and true (permissive) on any structural anomaly. For
// array-typed stored values every element is tested so {tags: "go"}
// matches a stored ["go", "rust"].
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

// scalarMatch coerces across int32/int64/double per mongo's numeric
// equivalence rules; non-numeric types match on native shape only.
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

// bsonNumericAsFloat returns ok=false for int64 values outside the
// float64-exact range so the predicate falls back to permissive.
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

// scanTopLevelBSONNumeric locates a numeric field and returns its
// float64 value. The status return distinguishes found, missing (range
// can prove no match), and bail (force permissive).
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
