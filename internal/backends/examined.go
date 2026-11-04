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

package backends

import (
	"context"
	"sync"
	"sync/atomic"
)

// ExaminedCounts accumulates the index keys and documents read by the
// operations run under the context it is attached to, as explain's
// executionStats report them.
type ExaminedCounts struct {
	keys atomic.Int64
	docs atomic.Int64

	mu       sync.Mutex
	branches []ExplainBranchCounts
}

func (e *ExaminedCounts) Keys() int64 { return e.keys.Load() }

func (e *ExaminedCounts) Docs() int64 { return e.docs.Load() }

// Branches returns the counts of each index scan under an OR or SORT_MERGE
// stage, in plan order.
func (e *ExaminedCounts) Branches() []ExplainBranchCounts {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ExplainBranchCounts(nil), e.branches...)
}

// ExplainBranchCounts are the index keys one scan under an OR or SORT_MERGE
// stage read and the entries it returned.
type ExplainBranchCounts struct {
	KeysExamined int64
	NReturned    int64
}

type examinedCountsKey struct{}

// WithExaminedCounts returns a context whose reads are counted in the returned
// ExaminedCounts.
func WithExaminedCounts(ctx context.Context) (context.Context, *ExaminedCounts) {
	e := new(ExaminedCounts)
	return context.WithValue(ctx, examinedCountsKey{}, e), e
}

// AddKeysExamined counts n index keys read under ctx.
func AddKeysExamined(ctx context.Context, n int) {
	if e, ok := ctx.Value(examinedCountsKey{}).(*ExaminedCounts); ok {
		e.keys.Add(int64(n))
	}
}

// AddBranchExamined counts keys read and returned entries for the branch'th
// index scan under an OR or SORT_MERGE stage run under ctx.
func AddBranchExamined(ctx context.Context, branch int, keys, returned int64) {
	e, ok := ctx.Value(examinedCountsKey{}).(*ExaminedCounts)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.branches) <= branch {
		e.branches = append(e.branches, ExplainBranchCounts{})
	}
	e.branches[branch].KeysExamined += keys
	e.branches[branch].NReturned += returned
}

// AddDocsExamined counts n documents read under ctx.
func AddDocsExamined(ctx context.Context, n int) {
	if e, ok := ctx.Value(examinedCountsKey{}).(*ExaminedCounts); ok {
		e.docs.Add(int64(n))
	}
}
