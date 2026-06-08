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

// Package bsonindexed provides a prolly-tree-backed indexed BSON document
// implementation analogous to dolt's IndexedJsonDocument. Stored BSON
// documents are sliced into byte chunks at content-defined boundaries and
// indexed by a Location -- a serialised path that orders by document
// traversal order. The indexed structure enables sub-linear point lookups
// and structural-sharing mutations across commits.
//
// This package is the foundation for the bson-a storage format: leaf
// chunks contain raw BSON byte substrings (with all container length
// prefixes intact), and mutations patch ancestor length prefixes
// whenever a container body's byte length changes.
package bsonindexed

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/mohae/uvarint"
)

// LocationKey is the serialised wire form of a Location: a byte sequence
// that lex-orders by document traversal order. Used as the key in the
// prolly tree's inner address-map nodes.
type LocationKey []byte

// EndOfDocumentKey is the chunk key used for the final chunk of a
// document. Encoded as a single 0xFF byte so it lex-sorts after every
// path-level key (whose state byte is < 0xFF). This sidesteps the
// prefix-ordering pitfall: a plain {EndOfValue} key would otherwise
// be a byte-prefix of all field-level end-of-value keys and sort
// first under naive lex-byte comparison.
var EndOfDocumentKey = LocationKey{0xFF}

// PathType is the state byte at the head of every Location key. The
// constants are ordered so that lexicographic byte comparison of
// LocationKey values matches the natural document traversal order:
// startOfValue < objectInitialElement < arrayInitialElement < middleOfValue < endOfValue.
type PathType byte

const (
	StartOfValue         PathType = iota // location points at the first byte of a value
	ObjectInitialElement                 // location points at the insertion point for the first element of an object
	ArrayInitialElement                  // location points at the insertion point for the first element of an array
	MiddleOfValue                        // location is partway through a string/binary value that spans a chunk boundary
	EndOfValue                           // location points one byte past the end of a value
	pathTypeNumElements
)

// Separator bytes used inside a LocationKey. 0xFF and 0xFE were chosen
// because they cannot appear in UTF-8, so they unambiguously mark the
// boundaries of object keys and array indices within the encoded path.
// BSON CString field names are UTF-8 with no embedded NUL, so 0xFF and
// 0xFE never appear inside a key fragment.
const (
	BeginObjectKey byte = 0xFF
	BeginArrayKey  byte = 0xFE
)

// ErrUnknownLocationKey reports that a LocationKey was written by a
// future version of the format and cannot be safely interpreted.
var ErrUnknownLocationKey = fmt.Errorf("indexed BSON document key was written by a future version; falling back to unoptimised path")

// ErrUnsupportedPath reports that a mongo dotted path uses syntax this
// implementation cannot handle without falling back (currently: nothing,
// but reserved for future operators).
var ErrUnsupportedPath = fmt.Errorf("indexed BSON document does not support this path; falling back to unoptimised implementation")

// Location is the in-memory representation of a path into a BSON
// document. It carries both the wire encoding (key) and a cache of
// path-element offsets so callers can navigate elements without
// re-scanning the key.
type Location struct {
	key     LocationKey
	offsets []int // index of each path element start in key, plus the key length as a final entry
}

// NewRootLocation returns a Location representing the start of the
// document root, with no path elements descended.
func NewRootLocation() Location {
	return Location{
		key:     []byte{byte(StartOfValue)},
		offsets: []int{1},
	}
}

// Clone returns a deep copy of the Location.
func (p Location) Clone() Location {
	return Location{
		key:     bytes.Clone(p.key),
		offsets: append([]int(nil), p.offsets...),
	}
}

// Key returns the raw LocationKey bytes. The returned slice aliases the
// internal buffer; callers that retain it across mutations should clone.
func (p Location) Key() LocationKey {
	return p.key
}

// KeyClone returns an independent copy of the LocationKey bytes.
func (p Location) KeyClone() LocationKey {
	return bytes.Clone(p.key)
}

// State returns the PathType byte at the head of the Location key.
func (p Location) State() PathType {
	if len(p.key) == 0 {
		return StartOfValue
	}
	return PathType(p.key[0])
}

// SetState replaces the state byte at the head of the Location key.
func (p *Location) SetState(t PathType) {
	if len(p.key) == 0 {
		p.key = []byte{byte(t)}
		p.offsets = []int{1}
		return
	}
	p.key[0] = byte(t)
}

// Size returns the number of path elements descended below the root.
// A root location has size 0.
func (p Location) Size() int {
	return len(p.offsets) - 1
}

// AppendObjectKey extends the Location with an object key step. Reuses
// the underlying buffer when capacity allows.
func (p *Location) AppendObjectKey(key []byte) {
	p.key = append(p.key, BeginObjectKey)
	start := len(p.key)
	p.key = append(p.key, key...)
	p.offsets = append(p.offsets, start-1)
	p.offsets[len(p.offsets)-1] = start - 1
	p.refreshTail()
}

// AppendArrayIndex extends the Location with an array index step,
// encoded as a SQLite4 varint so lex order is preserved for indices.
func (p *Location) AppendArrayIndex(idx uint64) {
	p.key = append(p.key, BeginArrayKey)
	start := len(p.key)
	p.key = appendVarint(p.key, idx)
	p.offsets = append(p.offsets, start-1)
	p.offsets[len(p.offsets)-1] = start - 1
	p.refreshTail()
}

// Pop removes the deepest path element from the Location. The state
// byte is preserved; callers typically set it via SetState after a Pop.
func (p *Location) Pop() {
	if len(p.offsets) < 2 {
		return
	}
	elementStart := p.offsets[len(p.offsets)-2]
	p.key = p.key[:elementStart]
	p.offsets = p.offsets[:len(p.offsets)-1]
	p.offsets[len(p.offsets)-1] = len(p.key)
}

// refreshTail keeps the final offset entry equal to len(key). The
// invariant is that offsets has Size()+1 entries: one per path element
// plus a trailing entry that equals len(key).
func (p *Location) refreshTail() {
	if len(p.offsets) == 0 {
		return
	}
	p.offsets[len(p.offsets)-1] = len(p.key)
}

// PathElement describes one step in a Location: either an object key
// or an array index.
type PathElement struct {
	IsArrayIndex bool
	Key          []byte // valid when !IsArrayIndex; bytes of the field name
	idx          uint64 // valid when IsArrayIndex
}

// ArrayIndex returns the decoded array index. Panics if the element is
// an object key; check IsArrayIndex before calling.
func (e PathElement) ArrayIndex() uint64 {
	if !e.IsArrayIndex {
		panic("PathElement.ArrayIndex called on an object key element")
	}
	return e.idx
}

// PathElement returns the i-th path element descended below the root.
// i must satisfy 0 <= i < Size().
func (p Location) PathElement(i int) PathElement {
	start := p.offsets[i]
	end := p.offsets[i+1]
	return decodePathElement(p.key[start:end])
}

// LastPathElement returns the deepest path element. Panics on a root
// location; callers must check Size() > 0.
func (p Location) LastPathElement() PathElement {
	if p.Size() == 0 {
		panic("LastPathElement called on a root location")
	}
	return p.PathElement(p.Size() - 1)
}

// decodePathElement parses one path element from its serialised form
// (BeginObjectKey + key bytes, or BeginArrayKey + varint).
func decodePathElement(b []byte) PathElement {
	if len(b) == 0 {
		return PathElement{}
	}
	switch b[0] {
	case BeginObjectKey:
		return PathElement{IsArrayIndex: false, Key: b[1:]}
	case BeginArrayKey:
		idx, _ := uvarint.Uvarint(b[1:])
		return PathElement{IsArrayIndex: true, idx: idx}
	}
	return PathElement{}
}

// FromKey rebuilds a Location from its serialised key form.
func FromKey(key LocationKey) Location {
	loc := Location{
		key:     bytes.Clone(key),
		offsets: []int{},
	}
	i := 1
	for i < len(key) {
		switch key[i] {
		case BeginObjectKey:
			loc.offsets = append(loc.offsets, i)
			// Skip the separator and walk until the next separator or end.
			i++
			for i < len(key) && key[i] != BeginObjectKey && key[i] != BeginArrayKey {
				i++
			}
		case BeginArrayKey:
			loc.offsets = append(loc.offsets, i)
			i++
			i += varintLen(key[i])
		default:
			// Should not happen for well-formed keys; advance defensively.
			i++
		}
	}
	loc.offsets = append(loc.offsets, len(key))
	return loc
}

// Compare returns -1, 0, or +1 reflecting the lex order of two
// LocationKey values. This is the comparator the prolly tree uses to
// order address-map entries. Lex order of LocationKey corresponds to
// document-traversal order, with state-byte tie-breaking baked into the
// key prefix.
func Compare(a, b LocationKey) (int, error) {
	if len(a) == 0 || len(b) == 0 {
		if bytes.Equal(a, b) {
			return 0, nil
		}
		if len(a) == 0 {
			return -1, nil
		}
		return 1, nil
	}
	if PathType(a[0]) >= pathTypeNumElements || PathType(b[0]) >= pathTypeNumElements {
		return 0, ErrUnknownLocationKey
	}
	// Compare path-element-by-path-element, then resolve via state byte
	// when paths agree.
	ai, bi := 1, 1
	for ai < len(a) && bi < len(b) {
		aLen := elementLen(a[ai:])
		bLen := elementLen(b[bi:])
		// First compare the separator bytes: object-key sep (0xFF) and
		// array-key sep (0xFE) are distinct, so a mismatch resolves the
		// comparison via raw byte order.
		if a[ai] != b[bi] {
			if a[ai] < b[bi] {
				return -1, nil
			}
			return 1, nil
		}
		// Same separator: compare element bodies.
		body := bytes.Compare(a[ai+1:ai+aLen], b[bi+1:bi+bLen])
		if body != 0 {
			return body, nil
		}
		ai += aLen
		bi += bLen
	}
	if ai == len(a) && bi == len(b) {
		// Same path; compare state bytes. start<object<array<middle<end.
		return comparePathTypes(PathType(a[0]), PathType(b[0]))
	}
	// One key is a prefix of the other in path elements. The shorter is
	// the ancestor; resolve by its state byte:
	//   shorter is StartOfValue: ancestor comes first
	//   shorter is EndOfValue: ancestor comes last
	//   shorter is InitialElement: ancestor comes first (insertion point
	//     at object/array start is before any descended child)
	if ai == len(a) {
		// a is the ancestor of b
		switch PathType(a[0]) {
		case StartOfValue, ObjectInitialElement, ArrayInitialElement:
			return -1, nil
		case EndOfValue:
			return 1, nil
		}
		return -1, nil
	}
	// b is the ancestor of a
	switch PathType(b[0]) {
	case StartOfValue, ObjectInitialElement, ArrayInitialElement:
		return 1, nil
	case EndOfValue:
		return -1, nil
	}
	return 1, nil
}

// comparePathTypes orders state bytes when the path-element prefixes
// match. Order: start < object-initial < array-initial < middle < end.
func comparePathTypes(a, b PathType) (int, error) {
	if a == b {
		return 0, nil
	}
	if a < b {
		return -1, nil
	}
	return 1, nil
}

// elementLen returns the byte length of one encoded path element
// starting at b[0] (the separator byte). For an array index, decodes
// the varint length; for an object key, walks until the next separator
// or end-of-buffer.
func elementLen(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	switch b[0] {
	case BeginArrayKey:
		return 1 + varintLen(b[1])
	case BeginObjectKey:
		// Key body runs until the next separator.
		for i := 1; i < len(b); i++ {
			if b[i] == BeginObjectKey || b[i] == BeginArrayKey {
				return i
			}
		}
		return len(b)
	}
	return 1
}

// FromMongoPath parses a mongo dotted path ("a.b.c", "a.0.b") into a
// Location. Numeric components are encoded as array indices; all other
// components are encoded as object keys. The caller decides whether the
// resulting Location is sensible against a specific document shape.
//
// Empty path returns a root Location.
func FromMongoPath(path string) (Location, error) {
	loc := NewRootLocation()
	if path == "" {
		return loc, nil
	}
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return Location{}, fmt.Errorf("invalid mongo path %q: empty component", path)
		}
		if idx, err := strconv.ParseUint(part, 10, 64); err == nil {
			loc.AppendArrayIndex(idx)
			continue
		}
		loc.AppendObjectKey([]byte(part))
	}
	return loc, nil
}

// ToMongoPath returns the dotted-path representation of a Location.
// Array indices render as their decimal form, joined by dots like
// object keys: "a.0.b" rather than MySQL's "$.a[0].b".
func (p Location) ToMongoPath() string {
	if p.Size() == 0 {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < p.Size(); i++ {
		if i > 0 {
			sb.WriteByte('.')
		}
		el := p.PathElement(i)
		if el.IsArrayIndex {
			sb.WriteString(strconv.FormatUint(el.ArrayIndex(), 10))
		} else {
			sb.Write(el.Key)
		}
	}
	return sb.String()
}

// IsAncestor reports whether prefix encodes a Location that is an
// ancestor path of full. State bytes are ignored; only path-element
// prefixes are compared.
func IsAncestor(full, prefix LocationKey) bool {
	if len(prefix) <= 1 || len(full) <= 1 {
		return false
	}
	if len(full) < len(prefix) {
		return false
	}
	if !bytes.Equal(full[1:len(prefix)], prefix[1:]) {
		return false
	}
	if len(full) == len(prefix) {
		return false
	}
	// The character immediately after the prefix in full must be a
	// separator, otherwise we'd be matching "a" as a prefix of "aa".
	next := full[len(prefix)]
	return next == BeginObjectKey || next == BeginArrayKey
}

// ModifySameArray reports whether two LocationKey values point inside
// the same containing array. Used by merge logic to detect concurrent
// edits to the same array.
func ModifySameArray(a, b LocationKey) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	i := 1
	for i < len(a) && i < len(b) && a[i] == b[i] {
		if a[i] == BeginArrayKey {
			return true
		}
		i++
	}
	return false
}

// appendVarint writes a SQLite4 varint of x to b and returns the
// updated slice. SQLite4 varints have the property that lex byte order
// equals numeric order, so array indices sort naturally.
func appendVarint(b []byte, x uint64) []byte {
	tmp := make([]byte, 9)
	n := uvarint.Encode(tmp, x)
	return append(b, tmp[:n]...)
}

// varintLen returns the byte length of a SQLite4 varint given its first
// byte. Matches the encoding used by appendVarint and the dolt JSON
// location code.
func varintLen(firstByte byte) int {
	if firstByte <= 240 {
		return 1
	}
	if firstByte <= 248 {
		return 2
	}
	return int(firstByte - 246)
}

// PutUint32LE writes v in BSON's native little-endian byte order to
// the first four bytes of dst. Convenience wrapper used by the chunker
// when patching ancestor length prefixes.
func PutUint32LE(dst []byte, v uint32) {
	binary.LittleEndian.PutUint32(dst, v)
}

// ReadUint32LE reads a BSON length prefix from the first four bytes of
// src. Returns 0 if src is too short.
func ReadUint32LE(src []byte) uint32 {
	if len(src) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(src)
}
