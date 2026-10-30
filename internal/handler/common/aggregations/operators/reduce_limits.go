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

package operators

import (
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Limits mongod applies to the $reduce accumulator.
const (
	// maxReduceBytes matches internalQueryMaxMapFilterReduceBytes.
	maxReduceBytes = 100 * 1024 * 1024

	// maxReduceDepth is twice the BSON nesting limit, as in mongod.
	maxReduceDepth = 2 * bson.MaxNestingDepth

	// reduceDepthCheckInterval matches internalReduceAccumulatedValueDepthCheckInterval.
	reduceDepthCheckInterval = 16

	// maxReduceDepthCheckInterval caps the backoff, which mongod leaves
	// unbounded. Without it an accumulator that holds its depth for a while
	// can then nest millions of levels between checks and overflow the stack
	// of later recursive comparisons (workspace-8tj.6).
	maxReduceDepthCheckInterval = 1024
)

// reduceLimiter tracks mongod's periodic accumulator depth check, whose
// interval doubles each time the depth stops growing.
type reduceLimiter struct {
	iteration int
	interval  int
	prevDepth int
}

func newReduceLimiter() *reduceLimiter {
	return &reduceLimiter{interval: reduceDepthCheckInterval, prevDepth: -1}
}

func (l *reduceLimiter) check(accumulator any) error {
	defer func() { l.iteration++ }()

	if approximateValueSize(accumulator, maxReduceBytes) > maxReduceBytes {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrExceededMemoryLimit,
			"$reduce would use too much memory and cannot spill",
			"$reduce",
		)
	}

	if l.iteration%l.interval != 0 {
		return nil
	}

	switch accumulator.(type) {
	case *types.Document, *types.Array:
	default:
		return nil
	}

	depth := valueDepth(accumulator, maxReduceDepth, 0)
	if depth == -1 {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrOverflow,
			"$reduce accumulated value exceeded max allowable BSON depth",
			"$reduce",
		)
	}

	if depth == l.prevDepth {
		l.interval = min(l.interval*2, maxReduceDepthCheckInterval)
	}
	l.prevDepth = depth

	return nil
}

// valueDepth ports mongod's Value::depth: scalars and empty containers are 0,
// and -1 means some value sits at or below maxDepth.
func valueDepth(v any, maxDepth, curDepth int) int {
	if curDepth >= maxDepth {
		return -1
	}

	maxChildDepth := -1
	for _, child := range containerChildren(v) {
		childDepth := valueDepth(child, maxDepth, curDepth+1)
		if childDepth == -1 {
			return -1
		}
		maxChildDepth = max(maxChildDepth, childDepth)
	}

	return maxChildDepth + 1
}

// In-memory sizes mongod uses in Value::getApproximateSize, for x86-64.
const (
	valueBytes         = 16
	refCountedBytes    = 16
	vectorBytes        = 32
	documentBytes      = 64
	documentFieldBytes = 32
	shortStringMax     = 11
)

// approximateValueSize estimates mongod's Value::getApproximateSize. It stops
// counting once the total exceeds limit, so shared subvalues cannot make it
// walk an exponentially large tree.
func approximateValueSize(v any, limit int) int {
	size := valueBytes

	switch v := v.(type) {
	case string:
		size += stringStorageBytes(len(v))
	case types.Binary:
		size += stringStorageBytes(len(v.B))
	case types.Regex:
		size += stringStorageBytes(len(v.Pattern) + 1 + len(v.Options))
	case types.Decimal128:
		size += refCountedBytes
	case *types.Document:
		size += documentBytes
		for _, key := range v.Keys() {
			size += documentFieldBytes + len(key)
		}
		for _, field := range v.Values() {
			if size > limit {
				return size
			}
			size += approximateValueSize(field, limit-size) - valueBytes
		}
	case *types.Array:
		size += vectorBytes
		for _, elem := range containerChildren(v) {
			if size > limit {
				return size
			}
			size += approximateValueSize(elem, limit-size)
		}
	}

	return size
}

func containerChildren(v any) []any {
	switch v := v.(type) {
	case *types.Document:
		return v.Values()
	case *types.Array:
		children := make([]any, v.Len())
		for i := range children {
			children[i] = must.NotFail(v.Get(i))
		}
		return children
	default:
		return nil
	}
}

func stringStorageBytes(n int) int {
	if n <= shortStringMax {
		return 0
	}
	return refCountedBytes + n
}
