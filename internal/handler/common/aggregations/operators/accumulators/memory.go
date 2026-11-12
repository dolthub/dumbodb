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
)

// accumulatorMemoryLimit is mongod's limit on the values one accumulator
// keeps (internalQueryMaxPushBytes and its kin).
const accumulatorMemoryLimit = 100 * 1024 * 1024

// keptMemory is the size of the values an accumulator keeps.
type keptMemory struct {
	bytes int
}

// keep charges v, failing as mongod does once the kept values pass the
// limit.
func (m *keptMemory) keep(v any, op string) error {
	m.bytes += types.BSONValueSize(v)
	if m.bytes <= accumulatorMemoryLimit {
		return nil
	}
	msg := fmt.Sprintf("%s used too much memory and cannot spill to disk. Memory limit: %d bytes", op, accumulatorMemoryLimit)
	switch op {
	case "$firstN", "$lastN", "$minN", "$maxN", "$topN", "$bottomN", "$top", "$bottom":
		msg = fmt.Sprintf("Accumulator used too much memory and spilling to disk cannot reduce memory consumption any further. "+
			"Memory limit: %d bytes", accumulatorMemoryLimit)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrExceededMemoryLimit, msg, op)
}

// release returns v's charge when the accumulator drops it.
func (m *keptMemory) release(v any) {
	m.bytes -= types.BSONValueSize(v)
}
