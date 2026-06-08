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

// Package bsonindexed provides a prolly-tree-backed indexed BSON document.
// Documents are sliced into chunks at content-defined boundaries and
// indexed by a Location: a serialised path that lex-orders by document
// traversal order.
package bsonindexed

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/mohae/uvarint"
)

type LocationKey []byte

// EndOfDocumentKey is the chunk key for the final chunk of a document.
// 0xFF lex-sorts after every path-level key (whose state byte is < 0xFF);
// a plain EndOfValue would be a byte-prefix of all field-level end keys
// and sort first.
var EndOfDocumentKey = LocationKey{0xFF}

// PathType is the state byte at the head of every Location key. The
// constants are ordered so that lexicographic byte comparison of
// LocationKey values matches document traversal order.
type PathType byte

const (
	StartOfValue PathType = iota
	ObjectInitialElement
	ArrayInitialElement
	MiddleOfValue // partway through a string/binary value that spans a chunk boundary
	EndOfValue
	pathTypeNumElements
)

// 0xFF and 0xFE cannot appear in UTF-8, so they unambiguously mark the
// boundaries of object keys (which are UTF-8 CStrings) within an encoded
// path.
const (
	BeginObjectKey byte = 0xFF
	BeginArrayKey  byte = 0xFE
)

var ErrUnknownLocationKey = fmt.Errorf("indexed BSON document key was written by a future version; falling back to unoptimised path")
var ErrUnsupportedPath = fmt.Errorf("indexed BSON document does not support this path; falling back to unoptimised implementation")

type Location struct {
	key     LocationKey
	offsets []int // start of each path element in key, plus a trailing entry equal to len(key)
}

func NewRootLocation() Location {
	return Location{
		key:     []byte{byte(StartOfValue)},
		offsets: []int{1},
	}
}

func (p Location) Clone() Location {
	return Location{
		key:     bytes.Clone(p.key),
		offsets: append([]int(nil), p.offsets...),
	}
}

// Key aliases the internal buffer; clone before retaining across mutations.
func (p Location) Key() LocationKey {
	return p.key
}

func (p Location) KeyClone() LocationKey {
	return bytes.Clone(p.key)
}

func (p Location) State() PathType {
	if len(p.key) == 0 {
		return StartOfValue
	}
	return PathType(p.key[0])
}

func (p *Location) SetState(t PathType) {
	if len(p.key) == 0 {
		p.key = []byte{byte(t)}
		p.offsets = []int{1}
		return
	}
	p.key[0] = byte(t)
}

func (p Location) Size() int {
	return len(p.offsets) - 1
}

func (p *Location) AppendObjectKey(key []byte) {
	p.key = append(p.key, BeginObjectKey)
	start := len(p.key)
	p.key = append(p.key, key...)
	p.offsets = append(p.offsets, start-1)
	p.offsets[len(p.offsets)-1] = start - 1
	p.refreshTail()
}

// AppendArrayIndex appends an index encoded as a SQLite4 varint so lex
// order is preserved across indices.
func (p *Location) AppendArrayIndex(idx uint64) {
	p.key = append(p.key, BeginArrayKey)
	start := len(p.key)
	p.key = appendVarint(p.key, idx)
	p.offsets = append(p.offsets, start-1)
	p.offsets[len(p.offsets)-1] = start - 1
	p.refreshTail()
}

func (p *Location) Pop() {
	if len(p.offsets) < 2 {
		return
	}
	elementStart := p.offsets[len(p.offsets)-2]
	p.key = p.key[:elementStart]
	p.offsets = p.offsets[:len(p.offsets)-1]
	p.offsets[len(p.offsets)-1] = len(p.key)
}

func (p *Location) refreshTail() {
	if len(p.offsets) == 0 {
		return
	}
	p.offsets[len(p.offsets)-1] = len(p.key)
}

type PathElement struct {
	IsArrayIndex bool
	Key          []byte // valid when !IsArrayIndex
	idx          uint64 // valid when IsArrayIndex
}

// ArrayIndex panics if the element is an object key.
func (e PathElement) ArrayIndex() uint64 {
	if !e.IsArrayIndex {
		panic("PathElement.ArrayIndex called on an object key element")
	}
	return e.idx
}

// PathElement returns the i-th element below the root (0 <= i < Size()).
func (p Location) PathElement(i int) PathElement {
	start := p.offsets[i]
	end := p.offsets[i+1]
	return decodePathElement(p.key[start:end])
}

// LastPathElement panics on a root location; check Size() > 0.
func (p Location) LastPathElement() PathElement {
	if p.Size() == 0 {
		panic("LastPathElement called on a root location")
	}
	return p.PathElement(p.Size() - 1)
}

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
			i++
			for i < len(key) && key[i] != BeginObjectKey && key[i] != BeginArrayKey {
				i++
			}
		case BeginArrayKey:
			loc.offsets = append(loc.offsets, i)
			i++
			i += varintLen(key[i])
		default:
			i++
		}
	}
	loc.offsets = append(loc.offsets, len(key))
	return loc
}

// Compare orders by document-traversal: lex order on LocationKey.
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
	ai, bi := 1, 1
	for ai < len(a) && bi < len(b) {
		aLen := elementLen(a[ai:])
		bLen := elementLen(b[bi:])
		if a[ai] != b[bi] {
			if a[ai] < b[bi] {
				return -1, nil
			}
			return 1, nil
		}
		body := bytes.Compare(a[ai+1:ai+aLen], b[bi+1:bi+bLen])
		if body != 0 {
			return body, nil
		}
		ai += aLen
		bi += bLen
	}
	if ai == len(a) && bi == len(b) {
		return comparePathTypes(PathType(a[0]), PathType(b[0]))
	}
	// One key is a path-prefix of the other; resolve by the shorter's state.
	if ai == len(a) {
		switch PathType(a[0]) {
		case StartOfValue, ObjectInitialElement, ArrayInitialElement:
			return -1, nil
		case EndOfValue:
			return 1, nil
		}
		return -1, nil
	}
	switch PathType(b[0]) {
	case StartOfValue, ObjectInitialElement, ArrayInitialElement:
		return 1, nil
	case EndOfValue:
		return -1, nil
	}
	return 1, nil
}

func comparePathTypes(a, b PathType) (int, error) {
	if a == b {
		return 0, nil
	}
	if a < b {
		return -1, nil
	}
	return 1, nil
}

func elementLen(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	switch b[0] {
	case BeginArrayKey:
		return 1 + varintLen(b[1])
	case BeginObjectKey:
		for i := 1; i < len(b); i++ {
			if b[i] == BeginObjectKey || b[i] == BeginArrayKey {
				return i
			}
		}
		return len(b)
	}
	return 1
}

// FromMongoPath: numeric components become array indices.
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

// IsAncestor ignores state bytes.
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
	// "a" must not match as a prefix of "aa".
	next := full[len(prefix)]
	return next == BeginObjectKey || next == BeginArrayKey
}

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

// appendVarint uses SQLite4 varint encoding so lex byte order equals
// numeric order; array indices sort naturally.
func appendVarint(b []byte, x uint64) []byte {
	tmp := make([]byte, 9)
	n := uvarint.Encode(tmp, x)
	return append(b, tmp[:n]...)
}

func varintLen(firstByte byte) int {
	if firstByte <= 240 {
		return 1
	}
	if firstByte <= 248 {
		return 2
	}
	return int(firstByte - 246)
}

func PutUint32LE(dst []byte, v uint32) {
	binary.LittleEndian.PutUint32(dst, v)
}

func ReadUint32LE(src []byte) uint32 {
	if len(src) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(src)
}
