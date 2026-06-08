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
	"fmt"
	"sort"
	"strconv"
)

// SetField writes valueBytes at path. valueBytes must already be in BSON
// wire form for typeByte (e.g. int32-length + bytes + 0x00 for a string).
// The path's parent container must exist; missing intermediates return an
// error.
func (d IndexedBsonDocument) SetField(ctx context.Context, path string, typeByte byte, valueBytes []byte) (IndexedBsonDocument, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	if loc.Size() == 0 {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: SetField requires a non-empty path")
	}
	buf, err := d.Bytes(ctx)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	newBuf, err := setFieldInBytes(buf, loc, typeByte, valueBytes)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	return Serialize(ctx, d.ns, newBuf)
}

// UnsetField is a no-op if path doesn't exist.
func (d IndexedBsonDocument) UnsetField(ctx context.Context, path string) (IndexedBsonDocument, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	if loc.Size() == 0 {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: UnsetField requires a non-empty path")
	}
	buf, err := d.Bytes(ctx)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	newBuf, err := unsetFieldInBytes(buf, loc)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	return Serialize(ctx, d.ns, newBuf)
}

func (d IndexedBsonDocument) PushArray(ctx context.Context, path string, elemType byte, elemValue []byte) (IndexedBsonDocument, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	if loc.Size() == 0 {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: PushArray requires a non-empty path")
	}
	buf, err := d.Bytes(ctx)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	newBuf, err := pushArrayInBytes(buf, loc, elemType, elemValue)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	return Serialize(ctx, d.ns, newBuf)
}

// PopArray is a no-op on an empty array.
func (d IndexedBsonDocument) PopArray(ctx context.Context, path string) (IndexedBsonDocument, error) {
	if len(path) > 1 && path[0] == '$' && path[1] == '.' {
		path = path[2:]
	}
	loc, err := FromMongoPath(path)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	if loc.Size() == 0 {
		return IndexedBsonDocument{}, fmt.Errorf("bsonindexed: PopArray requires a non-empty path")
	}
	buf, err := d.Bytes(ctx)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	newBuf, err := popArrayInBytes(buf, loc)
	if err != nil {
		return IndexedBsonDocument{}, err
	}
	return Serialize(ctx, d.ns, newBuf)
}

// pathWalk descends to the parent of target's last element. parentStart
// is the parent's length-prefix offset; parentEnd is the trailing 0x00.
// ancestorPrefixOffsets lists every container length-prefix from root to
// parent inclusive, in descent order.
func pathWalk(buf []byte, target Location) (parentStart, parentEnd int, parentIsArray bool, ancestorPrefixOffsets []int, err error) {
	if target.Size() == 0 {
		return 0, 0, false, nil, fmt.Errorf("bsonindexed: pathWalk requires a non-empty path")
	}
	cursorStart := 0
	cursorIsArray := false
	prefixes := []int{0}
	for i := 0; i < target.Size()-1; i++ {
		el := target.PathElement(i)
		typeByte, valueStart, valueEnd, ok := findFieldWithOffsets(buf, cursorStart, el)
		if !ok {
			return 0, 0, false, nil, fmt.Errorf("bsonindexed: path %q does not exist (failed at element %d)", target.ToMongoPath(), i)
		}
		if typeByte != typeDocument && typeByte != typeArray {
			return 0, 0, false, nil, fmt.Errorf("bsonindexed: path %q intermediate element is not a container", target.ToMongoPath())
		}
		cursorStart = valueStart
		cursorIsArray = typeByte == typeArray
		prefixes = append(prefixes, valueStart)
		_ = valueEnd
	}
	containerLen := int(binary.LittleEndian.Uint32(buf[cursorStart:]))
	return cursorStart, cursorStart + containerLen - 1, cursorIsArray, prefixes, nil
}

func findFieldWithOffsets(buf []byte, container int, el PathElement) (byte, int, int, bool) {
	if container+5 > len(buf) {
		return 0, 0, 0, false
	}
	containerLen := int(binary.LittleEndian.Uint32(buf[container:]))
	if container+containerLen > len(buf) {
		return 0, 0, 0, false
	}
	end := container + containerLen - 1
	pos := container + 4
	for pos < end {
		typeByte := buf[pos]
		if typeByte == 0x00 {
			return 0, 0, 0, false
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && buf[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			return 0, 0, 0, false
		}
		valueStart := nameEnd + 1
		valueEnd, ok := elementValueEnd(buf, valueStart, typeByte, end)
		if !ok {
			return 0, 0, 0, false
		}
		if matchElement(buf[nameStart:nameEnd], el) {
			return typeByte, valueStart, valueEnd, true
		}
		pos = valueEnd
	}
	return 0, 0, 0, false
}

// findFieldEntryRange returns the [type, name, value] byte range for the
// field at el so the caller can splice the entire entry.
func findFieldEntryRange(buf []byte, container int, el PathElement) (entryStart, entryEnd, valueStart int, ok bool) {
	if container+5 > len(buf) {
		return 0, 0, 0, false
	}
	containerLen := int(binary.LittleEndian.Uint32(buf[container:]))
	if container+containerLen > len(buf) {
		return 0, 0, 0, false
	}
	end := container + containerLen - 1
	pos := container + 4
	for pos < end {
		typeByte := buf[pos]
		if typeByte == 0x00 {
			return 0, 0, 0, false
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && buf[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			return 0, 0, 0, false
		}
		valueStartLocal := nameEnd + 1
		valueEnd, ok := elementValueEnd(buf, valueStartLocal, typeByte, end)
		if !ok {
			return 0, 0, 0, false
		}
		if matchElement(buf[nameStart:nameEnd], el) {
			return pos, valueEnd, valueStartLocal, true
		}
		pos = valueEnd
	}
	return 0, 0, 0, false
}

// findInsertionPoint returns the offset where a new field with newName
// should be inserted to keep lex order. Arrays always append.
func findInsertionPoint(buf []byte, container int, newName []byte, isArray bool) int {
	containerLen := int(binary.LittleEndian.Uint32(buf[container:]))
	end := container + containerLen - 1
	if isArray {
		return end
	}
	pos := container + 4
	for pos < end {
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && buf[nameEnd] != 0x00 {
			nameEnd++
		}
		if bytes.Compare(buf[nameStart:nameEnd], newName) > 0 {
			return pos
		}
		valueStart := nameEnd + 1
		valueEnd, ok := elementValueEnd(buf, valueStart, buf[pos], end)
		if !ok {
			return end
		}
		pos = valueEnd
	}
	return end
}

func setFieldInBytes(buf []byte, target Location, typeByte byte, valueBytes []byte) ([]byte, error) {
	parentStart, _, parentIsArray, ancestors, err := pathWalk(buf, target)
	if err != nil {
		return nil, err
	}
	leaf := target.PathElement(target.Size() - 1)
	leafName := elementNameBytes(leaf)

	if leaf.IsArrayIndex && !parentIsArray {
		return nil, fmt.Errorf("bsonindexed: SetField on array index path %q targets a non-array parent", target.ToMongoPath())
	}

	entryStart, entryEnd, _, found := findFieldEntryRange(buf, parentStart, leaf)
	newEntry := makeFieldEntry(typeByte, leafName, valueBytes)
	if found {
		return spliceAndPatch(buf, entryStart, entryEnd, newEntry, ancestors), nil
	}
	insertAt := findInsertionPoint(buf, parentStart, leafName, parentIsArray)
	return spliceAndPatch(buf, insertAt, insertAt, newEntry, ancestors), nil
}

func unsetFieldInBytes(buf []byte, target Location) ([]byte, error) {
	parentStart, _, parentIsArray, ancestors, err := pathWalk(buf, target)
	if err != nil {
		return nil, err
	}
	leaf := target.PathElement(target.Size() - 1)
	if leaf.IsArrayIndex && !parentIsArray {
		return nil, fmt.Errorf("bsonindexed: UnsetField on array index path %q targets a non-array parent", target.ToMongoPath())
	}
	entryStart, entryEnd, _, found := findFieldEntryRange(buf, parentStart, leaf)
	if !found {
		return buf, nil
	}
	return spliceAndPatch(buf, entryStart, entryEnd, nil, ancestors), nil
}

func pushArrayInBytes(buf []byte, target Location, elemType byte, elemValue []byte) ([]byte, error) {
	parentStart, _, _, ancestors, err := pathWalk(buf, target)
	if err != nil {
		return nil, err
	}
	leaf := target.PathElement(target.Size() - 1)
	typeByte, valueStart, _, ok := findFieldWithOffsets(buf, parentStart, leaf)
	if !ok {
		return nil, fmt.Errorf("bsonindexed: PushArray path %q does not exist", target.ToMongoPath())
	}
	if typeByte != typeArray {
		return nil, fmt.Errorf("bsonindexed: PushArray path %q is not an array (type 0x%02x)", target.ToMongoPath(), typeByte)
	}
	arrayStart := valueStart
	arrayLen := int(binary.LittleEndian.Uint32(buf[arrayStart:]))
	arrayEnd := arrayStart + arrayLen - 1
	nextIdx := countArrayElements(buf, arrayStart)
	name := []byte(strconv.FormatUint(uint64(nextIdx), 10))
	newEntry := makeFieldEntry(elemType, name, elemValue)
	allAncestors := append(append([]int(nil), ancestors...), arrayStart)
	return spliceAndPatch(buf, arrayEnd, arrayEnd, newEntry, allAncestors), nil
}

func popArrayInBytes(buf []byte, target Location) ([]byte, error) {
	parentStart, _, _, ancestors, err := pathWalk(buf, target)
	if err != nil {
		return nil, err
	}
	leaf := target.PathElement(target.Size() - 1)
	typeByte, valueStart, _, ok := findFieldWithOffsets(buf, parentStart, leaf)
	if !ok {
		return buf, nil
	}
	if typeByte != typeArray {
		return nil, fmt.Errorf("bsonindexed: PopArray path %q is not an array (type 0x%02x)", target.ToMongoPath(), typeByte)
	}
	arrayStart := valueStart
	count := countArrayElements(buf, arrayStart)
	if count == 0 {
		return buf, nil
	}
	lastName := []byte(strconv.FormatUint(uint64(count-1), 10))
	entryStart, entryEnd, _, found := findFieldEntryRange(buf, arrayStart, PathElement{IsArrayIndex: false, Key: lastName})
	if !found {
		return buf, nil
	}
	allAncestors := append(append([]int(nil), ancestors...), arrayStart)
	return spliceAndPatch(buf, entryStart, entryEnd, nil, allAncestors), nil
}

// spliceAndPatch replaces buf[start:end] with replacement and adjusts
// each ancestor container's length prefix by the delta. ancestors must
// not include offsets >= start.
func spliceAndPatch(buf []byte, start, end int, replacement []byte, ancestors []int) []byte {
	delta := len(replacement) - (end - start)
	if delta == 0 && len(replacement) == 0 {
		return buf
	}
	newBuf := make([]byte, 0, len(buf)+delta)
	newBuf = append(newBuf, buf[:start]...)
	newBuf = append(newBuf, replacement...)
	newBuf = append(newBuf, buf[end:]...)
	sort.Ints(ancestors)
	for _, off := range ancestors {
		if off+4 > len(newBuf) {
			continue
		}
		oldLen := binary.LittleEndian.Uint32(newBuf[off:])
		binary.LittleEndian.PutUint32(newBuf[off:], oldLen+uint32(delta))
	}
	return newBuf
}

func countArrayElements(buf []byte, arrayStart int) int {
	arrayLen := int(binary.LittleEndian.Uint32(buf[arrayStart:]))
	end := arrayStart + arrayLen - 1
	count := 0
	pos := arrayStart + 4
	for pos < end {
		typeByte := buf[pos]
		if typeByte == 0x00 {
			break
		}
		nameStart := pos + 1
		nameEnd := nameStart
		for nameEnd < end && buf[nameEnd] != 0x00 {
			nameEnd++
		}
		if nameEnd >= end {
			break
		}
		valueStart := nameEnd + 1
		valueEnd, ok := elementValueEnd(buf, valueStart, typeByte, end)
		if !ok {
			break
		}
		count++
		pos = valueEnd
	}
	return count
}

func makeFieldEntry(typeByte byte, name []byte, value []byte) []byte {
	out := make([]byte, 0, 2+len(name)+len(value))
	out = append(out, typeByte)
	out = append(out, name...)
	out = append(out, 0x00)
	out = append(out, value...)
	return out
}

func elementNameBytes(el PathElement) []byte {
	if el.IsArrayIndex {
		return []byte(strconv.FormatUint(el.ArrayIndex(), 10))
	}
	return el.Key
}
