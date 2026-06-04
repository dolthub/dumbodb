// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bsonindexed

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strconv"
)

// LookupResult is the typed return from a Lookup. TypeByte holds the
// BSON type byte for the located field; Value holds the raw BSON
// value bytes (without the type byte or field name). For nested
// container types (document, array), Value is the entire container
// including its 4-byte length prefix and trailing 0x00.
//
// Found is false if the path does not exist in the document; in that
// case the other fields are zero values.
type LookupResult struct {
	TypeByte byte
	Value    []byte
	Found    bool
}

// Lookup returns the field at the given mongo dotted path. The path
// is parsed via FromMongoPath, then the document is walked top-down
// to locate the field. A leading "$." prefix is accepted for MySQL
// path parity but stripped before parsing.
//
// Implementation: the document is materialised once via Bytes(), then
// walked by Scanner. For very large documents this is O(doc bytes);
// future optimisations may use AddressMap to fast-skip to the chunk
// containing the target path.
func (d IndexedBsonDocument) Lookup(ctx context.Context, path string) (LookupResult, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return LookupResult{}, err
	}
	buf, err := d.Bytes(ctx)
	if err != nil {
		return LookupResult{}, err
	}
	return lookupInBytes(buf, loc), nil
}

// lookupInBytes implements the path walk over fully materialised BSON
// bytes. Exported via Lookup; broken out for direct testing.
func lookupInBytes(buf []byte, target Location) LookupResult {
	// Root lookup returns the whole document.
	if target.Size() == 0 {
		return LookupResult{TypeByte: typeDocument, Value: buf, Found: true}
	}
	// Descend the path element-by-element, jumping into nested
	// containers as needed. Each step locates a field within the
	// current container.
	cursor := buf
	for i := 0; i < target.Size(); i++ {
		el := target.PathElement(i)
		typeByte, valueBytes, ok := findField(cursor, el)
		if !ok {
			return LookupResult{}
		}
		if i == target.Size()-1 {
			return LookupResult{TypeByte: typeByte, Value: valueBytes, Found: true}
		}
		// Step into nested container. Only documents and arrays are
		// traversable; scalars at non-terminal positions fail the
		// lookup.
		if typeByte != typeDocument && typeByte != typeArray {
			return LookupResult{}
		}
		cursor = valueBytes
	}
	return LookupResult{}
}

// findField scans the top-level fields of a BSON container (document
// or array bytes, including the 4-byte length prefix and trailing
// 0x00) for a single path element. Returns the field's type byte and
// value bytes (excluding the type byte and field name; the value
// alone), and a found flag.
//
// For an array container, the path element is treated as an array
// index and matched against the stringified index that BSON arrays
// use as field names.
func findField(container []byte, el PathElement) (byte, []byte, bool) {
	if len(container) < 5 {
		return 0, nil, false
	}
	containerLen := int(binary.LittleEndian.Uint32(container))
	if containerLen > len(container) {
		return 0, nil, false
	}
	end := containerLen - 1 // points at trailing 0x00
	pos := 4
	for pos < end {
		typeByte := container[pos]
		if typeByte == 0x00 {
			return 0, nil, false
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && container[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			return 0, nil, false
		}
		name := container[nameStart:nameEnd]
		valueStart := nameEnd + 1
		valueEnd, ok := elementValueEnd(container, valueStart, typeByte, end)
		if !ok {
			return 0, nil, false
		}
		if matchElement(name, el) {
			return typeByte, container[valueStart:valueEnd], true
		}
		pos = valueEnd
	}
	return 0, nil, false
}

// matchElement reports whether the BSON field name matches the
// PathElement. Object keys match the bytes directly; array indices
// match the stringified index.
func matchElement(name []byte, el PathElement) bool {
	if el.IsArrayIndex {
		return bytes.Equal(name, []byte(strconv.FormatUint(el.ArrayIndex(), 10)))
	}
	return bytes.Equal(name, el.Key)
}

// elementValueEnd computes the byte offset just past the end of the
// value starting at valueStart, given its type byte. Returns false
// when the buffer is too short. Mirrors the per-type sizing logic in
// Scanner.consumeValueBody but as a one-shot computation.
func elementValueEnd(buf []byte, valueStart int, typeByte byte, hardEnd int) (int, bool) {
	switch typeByte {
	case typeDouble, typeDate, typeTime, typeInt64:
		end := valueStart + 8
		return end, end <= hardEnd+1
	case typeInt32:
		end := valueStart + 4
		return end, end <= hardEnd+1
	case typeBool:
		end := valueStart + 1
		return end, end <= hardEnd+1
	case typeObjectID:
		end := valueStart + 12
		return end, end <= hardEnd+1
	case typeNull, typeUndef, typeMinKey, typeMaxKey:
		return valueStart, true
	case typeString, typeSymbol, typeJSCode:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		strLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + 4 + strLen
		return end, end <= hardEnd+1
	case typeBinary:
		if valueStart+5 > len(buf) {
			return 0, false
		}
		binLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + 5 + binLen
		return end, end <= hardEnd+1
	case typeDecimal:
		end := valueStart + 16
		return end, end <= hardEnd+1
	case typeRegex:
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
	case typeDBPtr:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		strLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + 4 + strLen + 12
		return end, end <= hardEnd+1
	case typeJSCodeW:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		totalLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + totalLen
		return end, end <= hardEnd+1
	case typeDocument, typeArray:
		if valueStart+4 > len(buf) {
			return 0, false
		}
		containerLen := int(binary.LittleEndian.Uint32(buf[valueStart:]))
		end := valueStart + containerLen
		return end, end <= hardEnd+1
	}
	return 0, false
}

// Has reports whether the given mongo dotted path exists in the
// document. Convenience wrapper over Lookup that discards the
// returned value.
func (d IndexedBsonDocument) Has(ctx context.Context, path string) (bool, error) {
	r, err := d.Lookup(ctx, path)
	if err != nil {
		return false, err
	}
	return r.Found, nil
}

// LookupRawBytes is a streaming-friendly helper that walks bsonBytes
// directly without materialising a separate copy. Used by the
// prefilter path to avoid an extra allocation when the caller already
// has the document bytes in hand.
func LookupRawBytes(bsonBytes []byte, path string) (LookupResult, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return LookupResult{}, err
	}
	return lookupInBytes(bsonBytes, loc), nil
}

// ensure io is referenced to keep the import in case we later switch
// to a streaming implementation that needs it.
var _ = io.EOF
