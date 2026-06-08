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

package bsonindexed

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
)

// LookupResult.Value carries the raw BSON value bytes (no type byte or
// field name). For containers, Value is the entire container including
// its length prefix and trailing 0x00.
type LookupResult struct {
	TypeByte byte
	Value    []byte
	Found    bool
}

// Lookup returns the field at a mongo dotted path. A leading "$." is
// accepted for MySQL parity.
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

func lookupInBytes(buf []byte, target Location) LookupResult {
	if target.Size() == 0 {
		return LookupResult{TypeByte: typeDocument, Value: buf, Found: true}
	}
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
		if typeByte != typeDocument && typeByte != typeArray {
			return LookupResult{}
		}
		cursor = valueBytes
	}
	return LookupResult{}
}

func findField(container []byte, el PathElement) (byte, []byte, bool) {
	if len(container) < 5 {
		return 0, nil, false
	}
	containerLen := int(binary.LittleEndian.Uint32(container))
	if containerLen > len(container) {
		return 0, nil, false
	}
	end := containerLen - 1
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

func matchElement(name []byte, el PathElement) bool {
	if el.IsArrayIndex {
		return bytes.Equal(name, []byte(strconv.FormatUint(el.ArrayIndex(), 10)))
	}
	return bytes.Equal(name, el.Key)
}

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

func (d IndexedBsonDocument) Has(ctx context.Context, path string) (bool, error) {
	r, err := d.Lookup(ctx, path)
	if err != nil {
		return false, err
	}
	return r.Found, nil
}

// LookupRawBytes avoids the extra materialisation when the caller
// already has the document bytes.
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
