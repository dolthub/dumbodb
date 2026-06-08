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

// BSON element type bytes (https://bsonspec.org/spec.html).
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

var ErrInvalidBSON = fmt.Errorf("invalid BSON byte stream")

// Scanner walks a BSON byte stream incrementally, emitting a Location at
// every value boundary in document-traversal order. AdvanceToNextLocation
// returns io.EOF when more bytes are needed or the document is complete.
type Scanner struct {
	buf         []byte
	pos         int
	path        Location
	stack       []frame
	rootInited  bool
	rootEndOffs int
	currType    byte
}

type frame struct {
	isArray  bool
	arrayIdx uint64
	endOff   int // absolute offset of the container's trailing 0x00
}

func NewScanner(buf []byte) *Scanner {
	return &Scanner{
		buf:  buf,
		pos:  0,
		path: NewRootLocation(),
	}
}

func (s *Scanner) SetBuffer(buf []byte) { s.buf = buf }
func (s *Scanner) Buffer() []byte       { return s.buf }
func (s *Scanner) Pos() int             { return s.pos }

// Path aliases internal state; KeyClone any LocationKey kept across
// scanner advances.
func (s *Scanner) Path() *Location { return &s.path }

func (s *Scanner) AtEnd() bool {
	return s.rootInited && len(s.stack) == 0 && s.path.State() == EndOfValue && s.pos >= s.rootEndOffs+1
}

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
		// Reserved for future streaming work; BSON scalars are currently
		// completed atomically so this state never arises.
		return fmt.Errorf("bsonindexed: scanner observed MiddleOfValue, which is unsupported")
	default:
		return fmt.Errorf("bsonindexed: scanner observed unknown state %d", s.path.State())
	}
}

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
	// BSON top-level is always an object.
	s.stack = append(s.stack, frame{isArray: false, endOff: s.rootEndOffs})
	if s.buf[s.pos] == 0x00 {
		s.path.SetState(EndOfValue)
		return nil
	}
	s.path.SetState(ObjectInitialElement)
	return nil
}

func (s *Scanner) startElement() error {
	if s.pos >= len(s.buf) {
		return io.EOF
	}
	top := &s.stack[len(s.stack)-1]
	tb := s.buf[s.pos]
	if tb == 0x00 {
		s.pos++
		s.stack = s.stack[:len(s.stack)-1]
		s.path.SetState(EndOfValue)
		return nil
	}
	nameStart := s.pos + 1
	nameEnd := nameStart
	for nameEnd < len(s.buf) && s.buf[nameEnd] != 0x00 {
		nameEnd++
	}
	if nameEnd >= len(s.buf) {
		return io.EOF
	}
	if top.isArray {
		s.path.AppendArrayIndex(top.arrayIdx)
		top.arrayIdx++
	} else {
		s.path.AppendObjectKey(s.buf[nameStart:nameEnd])
	}
	s.pos = nameEnd + 1
	s.currType = tb
	s.path.SetState(StartOfValue)
	return nil
}

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
			end++
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
		endOff := s.pos + containerLen - 1
		isArray := tb == typeArray
		s.pos += 4
		s.stack = append(s.stack, frame{isArray: isArray, endOff: endOff})
		if s.buf[s.pos] == 0x00 {
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

func (s *Scanner) afterEndOfValue() error {
	if len(s.stack) == 0 {
		return io.EOF
	}
	s.path.Pop()
	s.path.SetState(StartOfValue)
	return s.startElement()
}

func IndexNameForFrame(idx uint64) string {
	return strconv.FormatUint(idx, 10)
}
