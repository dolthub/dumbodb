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

// Mutation operations for the bson-a storage format. The byte-level
// implementation operates directly on raw BSON bytes: splice in a new
// field's bytes (or splice out an existing field's bytes), then patch
// each ancestor container's 4-byte little-endian length prefix by the
// net byte delta.
//
// The resulting bytes are handed back to Serialize for re-chunking.
// Chunks whose contents are unchanged across the mutation hash to the
// same blob and are deduplicated automatically by the underlying
// node store; only the chunks that actually changed get new hashes.
// This is the structural-sharing-across-history property the bson-a
// vs bson-b bake-off is designed to measure: under (a) every
// ancestor length-prefix change forces a new chunk hash at the chunk
// containing that prefix; under (b) the prefixes don't exist in the
// stored bytes so those chunks remain stable.

// SetField writes the given value at the path. The value bytes must
// be in BSON wire form for the given type byte (e.g., 4-byte little-
// endian int32 length + bytes + 0x00 for a string; 4-byte little-
// endian int32 itself for typeInt32). If the field exists at path it
// is overwritten; otherwise it is inserted at the lex-sorted position
// inside its parent container.
//
// The path's parent container must exist; intermediate path
// components must reference existing documents. Setting at a path
// whose parent doesn't exist (e.g. `a.b.c` when `a` is missing) is
// not supported here and returns an error -- callers must $set the
// parent path first.
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

// UnsetField removes the field at the path. If the path doesn't exist
// the document is returned unchanged.
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

// PushArray appends an element to the array at the path. The path
// must refer to an existing array; behaviour for non-existent or
// non-array paths is the BSON-level error from the walker.
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

// PopArray removes the last element of the array at the path.
// Returns the document unchanged if the array is empty or the path
// doesn't refer to an array.
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

// pathWalk descends buf to the parent container of the target path
// and returns:
//
//	parentStart: byte offset of the parent container's first byte
//	             (i.e. its 4-byte length prefix)
//	parentEnd:   byte offset of the parent's trailing 0x00 (exclusive
//	             upper bound of the container body bytes)
//	parentIsArray: whether the parent is a BSON array container
//	ancestorPrefixOffsets: byte offsets of each ancestor container's
//	             length-prefix (root first, deepest last). Includes
//	             the parent's own length prefix at the end.
//
// Path elements before the last are descended; the last element is
// not consumed here -- callers handle it (look up, insert, replace).
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

// findFieldWithOffsets is the byte-offset-aware variant of findField.
// container is the start offset (length-prefix byte) of a BSON
// container within buf. Returns the type byte, value start offset
// (absolute in buf), value end offset (absolute), and a found flag.
func findFieldWithOffsets(buf []byte, container int, el PathElement) (byte, int, int, bool) {
	if container+5 > len(buf) {
		return 0, 0, 0, false
	}
	containerLen := int(binary.LittleEndian.Uint32(buf[container:]))
	if container+containerLen > len(buf) {
		return 0, 0, 0, false
	}
	end := container + containerLen - 1 // trailing 0x00
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

// findFieldEntryRange locates the [type, name, value] byte range of
// the field at el within the container at containerStart. Returns
// (entryStart, entryEnd, valueStart, ok). Used by mutations that need
// to splice out or replace the entire field entry.
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

// findInsertionPoint returns the byte offset within container where
// a new field with the given name should be inserted to keep the
// container's fields in lex-sorted order. The returned offset is the
// byte where the new field's type byte should land. Used by SetField
// when the target field doesn't exist yet.
//
// For array containers (which use stringified indices as names), the
// insertion point is always the container's trailing 0x00: array
// extensions append. The caller decides whether arrays are allowed.
func findInsertionPoint(buf []byte, container int, newName []byte, isArray bool) int {
	containerLen := int(binary.LittleEndian.Uint32(buf[container:]))
	end := container + containerLen - 1 // trailing 0x00
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

// setFieldInBytes is the byte-splicing core of SetField. Walks to the
// parent container, finds or makes room for the leaf path element,
// splices in the new (type, name, value) entry, and patches each
// ancestor length prefix.
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
	// Field doesn't exist; insert at lex-sorted position (or append
	// for an array).
	insertAt := findInsertionPoint(buf, parentStart, leafName, parentIsArray)
	return spliceAndPatch(buf, insertAt, insertAt, newEntry, ancestors), nil
}

// unsetFieldInBytes is the byte-splicing core of UnsetField. If the
// field doesn't exist, returns the buffer unchanged.
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

// pushArrayInBytes appends one element to the BSON array at the
// target path. The element's name in BSON is the next stringified
// index (current array length).
func pushArrayInBytes(buf []byte, target Location, elemType byte, elemValue []byte) ([]byte, error) {
	parentStart, _, _, ancestors, err := pathWalk(buf, target)
	if err != nil {
		return nil, err
	}
	// Resolve the leaf: must point to an array within the parent.
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
	arrayEnd := arrayStart + arrayLen - 1 // trailing 0x00
	nextIdx := countArrayElements(buf, arrayStart)
	name := []byte(strconv.FormatUint(uint64(nextIdx), 10))
	newEntry := makeFieldEntry(elemType, name, elemValue)
	// Splice and include the array itself in the ancestor list (its
	// length prefix needs patching). pathWalk returned ancestors up
	// to but not including the array; append arrayStart.
	allAncestors := append(append([]int(nil), ancestors...), arrayStart)
	return spliceAndPatch(buf, arrayEnd, arrayEnd, newEntry, allAncestors), nil
}

// popArrayInBytes removes the last element of the BSON array at the
// target path. Returns the buffer unchanged if the array is empty.
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

// spliceAndPatch produces a new BSON document where buf[start:end] is
// replaced by replacement, and each ancestor container's length
// prefix (at the byte offsets in ancestors) is adjusted by the
// resulting length delta.
//
// ancestors must be sorted ascending and must NOT include offsets >=
// start (those would be inside the splice region and meaningless).
// The delta is len(replacement) - (end - start).
func spliceAndPatch(buf []byte, start, end int, replacement []byte, ancestors []int) []byte {
	delta := len(replacement) - (end - start)
	if delta == 0 && len(replacement) == 0 {
		// No-op; return buf unchanged.
		return buf
	}
	newBuf := make([]byte, 0, len(buf)+delta)
	newBuf = append(newBuf, buf[:start]...)
	newBuf = append(newBuf, replacement...)
	newBuf = append(newBuf, buf[end:]...)
	// Patch each ancestor length prefix. ancestors are absolute
	// offsets into buf; in newBuf the offsets are the same because
	// every ancestor is at offset < start. Sort defensively before
	// patching so we apply deepest-first... actually order doesn't
	// matter for length-prefix patching since each prefix is
	// independent.
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

// countArrayElements returns the number of top-level elements in the
// BSON array starting at arrayStart in buf. Used by PushArray to
// pick the next array index.
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

// makeFieldEntry composes a single BSON field entry from its type
// byte, name (as CString bytes without the trailing 0x00), and
// already-encoded value bytes.
func makeFieldEntry(typeByte byte, name []byte, value []byte) []byte {
	out := make([]byte, 0, 2+len(name)+len(value))
	out = append(out, typeByte)
	out = append(out, name...)
	out = append(out, 0x00)
	out = append(out, value...)
	return out
}

// elementNameBytes returns the byte form of a PathElement's name --
// the field name bytes for object keys, or the stringified index for
// array indices.
func elementNameBytes(el PathElement) []byte {
	if el.IsArrayIndex {
		return []byte(strconv.FormatUint(el.ArrayIndex(), 10))
	}
	return el.Key
}
