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

package bson

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/FerretDB/wire/wirebson"
	"github.com/stretchr/testify/require"
)

// nestedRaw encodes {a: {a: ... {} ...}} (or {a: [[...]]}) with depth levels
// including the outermost document. It is built front to back so huge depths
// cost linear time.
func nestedRaw(depth int, arrays bool) []byte {
	tag, name := byte(0x03), byte('a')
	if arrays {
		tag, name = 0x04, '0'
	}
	const levelOverhead = 4 + 3 + 1 // size, tag+name+NUL, terminator
	out := make([]byte, 0, depth*levelOverhead+5)
	for level := range depth - 1 {
		size := (depth-level-1)*levelOverhead + 5
		out = binary.LittleEndian.AppendUint32(out, uint32(size))
		if level == 0 {
			out = append(out, tag, 'a', 0)
		} else {
			out = append(out, tag, name, 0)
		}
	}
	out = append(out, 5, 0, 0, 0, 0)
	for range depth - 1 {
		out = append(out, 0)
	}
	return out
}

func TestToDocumentAcceptsMaxNestingDepth(t *testing.T) {
	_, err := ToDocument(wirebson.RawDocument(nestedRaw(MaxNestingDepth, false)))
	require.NoError(t, err)
}

func TestToDocumentRejectsExcessiveNesting(t *testing.T) {
	for _, arrays := range []bool{false, true} {
		_, err := ToDocument(wirebson.RawDocument(nestedRaw(MaxNestingDepth+2, arrays)))
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrNestingTooDeep), "arrays=%v: %v", arrays, err)
	}
}

// Deep enough to exhaust the goroutine stack if conversion recursed without
// a limit; a crash here takes down the whole process.
func TestToDocumentRejectsHugeNestingWithoutCrashing(t *testing.T) {
	_, err := ToDocument(wirebson.RawDocument(nestedRaw(2_000_000, false)))
	require.ErrorIs(t, err, ErrNestingTooDeep)
}
