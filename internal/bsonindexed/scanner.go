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
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
)

// BSON element type bytes, as defined by the BSON spec
// (https://bsonspec.org/spec.html).
const (
	typeDouble   byte = 0x01
	typeString   byte = 0x02
	typeDocument byte = 0x03
	typeArray    byte = 0x04
	typeBinary   byte = 0x05
	typeUndef    byte = 0x06
	typeObjectID byte = 0x07
	typeBool     byte = 0x08
	typeDate     byte = 0x09
	typeNull     byte = 0x0A
	typeRegex    byte = 0x0B
	typeDBPtr    byte = 0x0C
	typeJSCode   byte = 0x0D
	typeSymbol   byte = 0x0E
	typeJSCodeW  byte = 0x0F
	typeInt32    byte = 0x10
	typeTime     byte = 0x11
	typeInt64    byte = 0x12
	typeDecimal  byte = 0x13
	typeMinKey   byte = 0xFF
	typeMaxKey   byte = 0x7F
)

// ErrInvalidBSON reports that the byte stream does not parse as BSON.
// Callers should treat this as a fatal scan error; the index cannot be
// trusted past the point of the error.
var ErrInvalidBSON = fmt.Errorf("invalid BSON byte stream")

// Scanner walks a BSON byte stream, advancing to each named location
// (every value's start and end) in document-traversal order. It is
// incremental: bytes can be appended to the buffer as they arrive, and
// AdvanceToNextLocation returns io.EOF when more data is needed.
//
// The scanner keeps a stack of container frames so it can distinguish
// keys-as-strings (object containers) from keys-as-array-indices (array
// containers). At every value boundary the Location reflects the path
// down through nested containers using array-index encoding where
// appropriate.
type Scanner struct {
	buf         []byte
	pos         int      // index of the next byte to interpret
	path        Location // path to the current value, with state byte set appropriately
	stack       []frame  // container frames; bottom is the root document
	rootInited  bool     // whether the root document's length prefix has been read
	rootEndOffs int      // byte offset of the trailing 0x00 of the root document
	currType    byte     // BSON type byte for the value currently being parsed
}

// frame is one container on the scanner's stack. Records whether the
// container is an array (to decide between object-key and array-index
// path elements), the array element count so far, and the absolute byte
// offset of the container's trailing 0x00 terminator so the scanner
// knows when to pop.
type frame struct {
	isArray  bool
	arrayIdx uint64
	endOff   int
}

// NewScanner constructs a fresh Scanner over buf. The scanner starts
// positioned at the document root with state StartOfValue; the first
// AdvanceToNextLocation reads the root length prefix and either
// transitions to ObjectInitialElement (empty doc emits start->end
// directly) or starts walking elements.
func NewScanner(buf []byte) *Scanner {
	return &Scanner{
		buf:  buf,
		pos:  0,
		path: NewRootLocation(),
	}
}

// SetBuffer replaces the scanner's input buffer. Used by the chunker
// when bytes have been freed off the front of the buffer or to feed
// more bytes in.
func (s *Scanner) SetBuffer(buf []byte) { s.buf = buf }

// Buffer returns the underlying input buffer.
func (s *Scanner) Buffer() []byte { return s.buf }

// Pos returns the index of the next byte the scanner will read.
func (s *Scanner) Pos() int { return s.pos }

// Path returns a reference to the scanner's current Location. Callers
// who keep the LocationKey across scanner advances should KeyClone it.
func (s *Scanner) Path() *Location { return &s.path }

// AtEnd reports whether the scanner has consumed every byte of the
// root document.
func (s *Scanner) AtEnd() bool {
	return s.rootInited && len(s.stack) == 0 && s.path.State() == EndOfValue && s.pos >= s.rootEndOffs+1
}

// AdvanceToNextLocation advances the scanner to the next named
// location boundary in the document. Returns io.EOF when the input
// runs out before a boundary can be emitted, or when the document
// has been fully consumed.
func (s *Scanner) AdvanceToNextLocation() error {
	if !s.rootInited {
		return s.openRoot()
	}
	switch s.path.State() {
	case StartOfValue:
		return s.consumeValueBody()
	case EndOfValue:
		return s.afterEndOfValue()
	case ObjectInitialElement, ArrayInitialElement:
		return s.startElement()
	case MiddleOfValue:
		// MiddleOfValue is set when a chunk boundary truncates the
		// middle of a long scalar value. Currently the BSON scanner
		// completes scalar values atomically, so MiddleOfValue should
		// never be observed; reserved for future streaming work.
		return fmt.Errorf("bsonindexed: scanner observed MiddleOfValue, which is unsupported")
	default:
		return fmt.Errorf("bsonindexed: scanner observed unknown state %d", s.path.State())
	}
}

// openRoot reads the root document's 4-byte length prefix and either
// transitions to ObjectInitialElement (non-empty doc) or directly to
// EndOfValue (empty doc: prefix is 5, followed only by the terminator).
func (s *Scanner) openRoot() error {
	if len(s.buf)-s.pos < 5 {
		return io.EOF
	}
	docLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
	if docLen < 5 || s.pos+docLen > len(s.buf) {
		return io.EOF
	}
	s.rootInited = true
	s.rootEndOffs = s.pos + docLen - 1
	s.pos += 4
	// Root is always an object (BSON top-level is always a document).
	s.stack = append(s.stack, frame{isArray: false, endOff: s.rootEndOffs})
	if s.buf[s.pos] == 0x00 {
		// Empty document; jump straight to EndOfValue.
		s.path.SetState(EndOfValue)
		return nil
	}
	s.path.SetState(ObjectInitialElement)
	return nil
}

// startElement reads the type byte and field-name CString for the next
// element in the current container, pushes the element onto the path,
// and transitions to StartOfValue. The byte position lands just past
// the field-name terminator, ready for consumeValueBody to read the
// value's payload.
func (s *Scanner) startElement() error {
	if s.pos >= len(s.buf) {
		return io.EOF
	}
	top := &s.stack[len(s.stack)-1]
	tb := s.buf[s.pos]
	if tb == 0x00 {
		// End of container body. Pop and transition to EndOfValue at
		// the parent path.
		s.pos++
		s.stack = s.stack[:len(s.stack)-1]
		s.path.SetState(EndOfValue)
		return nil
	}
	// Read field name CString.
	nameStart := s.pos + 1
	nameEnd := nameStart
	for nameEnd < len(s.buf) && s.buf[nameEnd] != 0x00 {
		nameEnd++
	}
	if nameEnd >= len(s.buf) {
		return io.EOF
	}
	// Push path element. Arrays use the running array index; objects
	// use the field name bytes.
	if top.isArray {
		s.path.AppendArrayIndex(top.arrayIdx)
		top.arrayIdx++
	} else {
		s.path.AppendObjectKey(s.buf[nameStart:nameEnd])
	}
	s.pos = nameEnd + 1 // skip the CString terminator
	s.currType = tb
	s.path.SetState(StartOfValue)
	return nil
}

// consumeValueBody advances past the value bytes for the current
// element and lands on EndOfValue. For nested containers (document,
// array) this only positions the scanner at the container's first
// body byte and pushes a new frame; subsequent AdvanceToNextLocation
// calls walk the container.
func (s *Scanner) consumeValueBody() error {
	tb := s.currType
	switch tb {
	case typeDouble, typeDate, typeTime, typeInt64:
		if s.pos+8 > len(s.buf) {
			return io.EOF
		}
		s.pos += 8
	case typeInt32:
		if s.pos+4 > len(s.buf) {
			return io.EOF
		}
		s.pos += 4
	case typeBool:
		if s.pos+1 > len(s.buf) {
			return io.EOF
		}
		s.pos++
	case typeObjectID:
		if s.pos+12 > len(s.buf) {
			return io.EOF
		}
		s.pos += 12
	case typeNull, typeUndef, typeMinKey, typeMaxKey:
		// Zero-byte value.
	case typeString, typeSymbol, typeJSCode:
		if s.pos+4 > len(s.buf) {
			return io.EOF
		}
		strLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
		if s.pos+4+strLen > len(s.buf) {
			return io.EOF
		}
		s.pos += 4 + strLen
	case typeBinary:
		if s.pos+5 > len(s.buf) {
			return io.EOF
		}
		binLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
		if s.pos+5+binLen > len(s.buf) {
			return io.EOF
		}
		s.pos += 5 + binLen
	case typeDecimal:
		if s.pos+16 > len(s.buf) {
			return io.EOF
		}
		s.pos += 16
	case typeRegex:
		// CString pattern then CString flags.
		end := s.pos
		for k := 0; k < 2; k++ {
			for end < len(s.buf) && s.buf[end] != 0x00 {
				end++
			}
			if end >= len(s.buf) {
				return io.EOF
			}
			end++ // skip terminator
		}
		s.pos = end
	case typeDBPtr:
		if s.pos+4 > len(s.buf) {
			return io.EOF
		}
		strLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
		if s.pos+4+strLen+12 > len(s.buf) {
			return io.EOF
		}
		s.pos += 4 + strLen + 12
	case typeJSCodeW:
		if s.pos+4 > len(s.buf) {
			return io.EOF
		}
		totalLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
		if s.pos+totalLen > len(s.buf) {
			return io.EOF
		}
		s.pos += totalLen
	case typeDocument, typeArray:
		if s.pos+4 > len(s.buf) {
			return io.EOF
		}
		containerLen := int(binary.LittleEndian.Uint32(s.buf[s.pos:]))
		if containerLen < 5 || s.pos+containerLen > len(s.buf) {
			return ErrInvalidBSON
		}
		endOff := s.pos + containerLen - 1 // points at trailing 0x00
		isArray := tb == typeArray
		s.pos += 4
		s.stack = append(s.stack, frame{isArray: isArray, endOff: endOff})
		if s.buf[s.pos] == 0x00 {
			// Empty container -- transition straight to EndOfValue
			// after popping back to the parent state.
			s.pos++
			s.stack = s.stack[:len(s.stack)-1]
			s.path.SetState(EndOfValue)
			return nil
		}
		if isArray {
			s.path.SetState(ArrayInitialElement)
		} else {
			s.path.SetState(ObjectInitialElement)
		}
		return nil
	default:
		return fmt.Errorf("bsonindexed: unknown BSON type byte 0x%02x at offset %d", tb, s.pos)
	}
	s.path.SetState(EndOfValue)
	return nil
}

// afterEndOfValue handles the transition after an EndOfValue is
// emitted. It pops the path's deepest element and proceeds to the
// next element in the parent container.
func (s *Scanner) afterEndOfValue() error {
	if len(s.stack) == 0 {
		// We popped past the root; document is complete.
		return io.EOF
	}
	s.path.Pop()
	s.path.SetState(StartOfValue) // transient; startElement will adjust
	return s.startElement()
}

// IndexNameForFrame returns the stringified array index that BSON would
// use as a field name for the element at idx. Used by tools that need
// to emit BSON-shaped bytes back out from a Location, where array
// elements need their canonical string-keyed form.
func IndexNameForFrame(idx uint64) string {
	return strconv.FormatUint(idx, 10)
}
