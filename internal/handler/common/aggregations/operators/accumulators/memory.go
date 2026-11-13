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

package accumulators

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// accumulatorMemoryLimit is mongod's limit on the values one accumulator
// keeps (internalQueryMaxPushBytes and its kin).
const accumulatorMemoryLimit = 100 * 1024 * 1024

// keptMemory is the size of the values an accumulator keeps, approximated as
// mongod's engine that runs it does.
type keptMemory struct {
	bytes int
	count int
	sbe   bool
}

func (m *keptMemory) size(v any) int {
	if m.sbe {
		return sbeValueSize(v)
	}
	return classicValueSize(v)
}

// sbeValueSize approximates the memory of v in an SBE array slot: a type tag
// and an 8-byte value, and the heap of a long string, a binData, an array or
// a document.
func sbeValueSize(v any) int {
	const slot, header = 9, 24
	switch v := v.(type) {
	case string:
		if len(v) > 7 {
			return slot + len(v) + 5
		}
	case types.Binary:
		return slot + len(v.B) + 5
	case *types.Array:
		size := slot + header
		for i := range v.Len() {
			size += sbeValueSize(must.NotFail(v.Get(i)))
		}
		return size
	case *types.Document:
		size := slot + header
		keys, values := v.Keys(), v.Values()
		for i, k := range keys {
			size += len(k) + sbeValueSize(values[i])
		}
		return size
	}
	return slot
}

// classicValueSize approximates the memory of v as the classic engine's
// Value, 16 bytes and the heap of a long string, a binData, an array or a
// document.
func classicValueSize(v any) int {
	const value, vector, document = 16, 24, 64
	switch v := v.(type) {
	case string:
		if len(v) > 15 {
			return value + len(v) + 1
		}
	case types.Binary:
		return value + len(v.B)
	case *types.Array:
		size := value + vector
		for i := range v.Len() {
			size += classicValueSize(must.NotFail(v.Get(i)))
		}
		return size
	case *types.Document:
		size := value + document
		keys, values := v.Keys(), v.Values()
		for i, k := range keys {
			size += len(k) + classicValueSize(values[i])
		}
		return size
	}
	return value
}

// keep charges v, failing as mongod does once the kept values pass the
// limit.
func (m *keptMemory) keep(v any, op string) error {
	size := m.size(v)
	before, count := m.bytes, m.count
	m.bytes += size
	m.count++
	if m.bytes <= accumulatorMemoryLimit {
		return nil
	}
	msg := fmt.Sprintf("%s used too much memory and cannot spill to disk. Memory limit: %d bytes", op, accumulatorMemoryLimit)
	switch op {
	case "$push", "$addToSet":
		if m.sbe {
			kind := map[string]string{"$push": "array", "$addToSet": "set"}[op]
			msg = fmt.Sprintf("Used too much memory for a single %s. Memory limit: %d bytes. The %s contains %d elements "+
				"and is of size %d bytes. The element being added has size %d bytes.",
				kind, accumulatorMemoryLimit, kind, count, before, size)
		}
	case "$firstN", "$lastN", "$minN", "$maxN", "$topN", "$bottomN", "$top", "$bottom":
		msg = fmt.Sprintf("Accumulator used too much memory and spilling to disk cannot reduce memory consumption any further. "+
			"Memory limit: %d bytes", accumulatorMemoryLimit)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrExceededMemoryLimit, msg, op)
}

// release returns v's charge when the accumulator drops it.
func (m *keptMemory) release(v any) {
	m.bytes -= m.size(v)
	m.count--
}
