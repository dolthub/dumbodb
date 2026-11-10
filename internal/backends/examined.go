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
	// entries counts the index entries a single index scan returned.
	entries atomic.Int64

	mu       sync.Mutex
	branches []ExplainBranchCounts
}

func (e *ExaminedCounts) Keys() int64 { return e.keys.Load() }

func (e *ExaminedCounts) Docs() int64 { return e.docs.Load() }

// Entries is what an index scan returned: its entries inside the bounds,
// each document once, before any filter on the documents.
func (e *ExaminedCounts) Entries() int64 { return e.entries.Load() }

// AddEntries counts n index entries returned; a nil e counts nothing.
func (e *ExaminedCounts) AddEntries(n int) {
	if e != nil {
		e.entries.Add(int64(n))
	}
}

// Branches returns the counts of each index scan under an OR or SORT_MERGE
// stage, in plan order.
func (e *ExaminedCounts) Branches() []ExplainBranchCounts {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ExplainBranchCounts(nil), e.branches...)
}

// ExplainBranchCounts are the index keys one scan under an OR or SORT_MERGE
// stage read and the entries it returned, and for a scan whose documents are
// fetched and filtered under the OR, those fetched and those matching.
type ExplainBranchCounts struct {
	KeysExamined int64
	NReturned    int64
	DocsExamined int64
	Matched      int64
}

type examinedCountsKey struct{}

// WithExaminedCounts returns a context whose reads are counted in the returned
// ExaminedCounts.
func WithExaminedCounts(ctx context.Context) (context.Context, *ExaminedCounts) {
	e := new(ExaminedCounts)
	return context.WithValue(ctx, examinedCountsKey{}, e), e
}

// ExaminedCountsFrom returns the counts attached to ctx, or nil. A scan that
// counts every key or document resolves them once: a context lookup per item
// costs more than reading the item.
func ExaminedCountsFrom(ctx context.Context) *ExaminedCounts {
	e, _ := ctx.Value(examinedCountsKey{}).(*ExaminedCounts)
	return e
}

// AddKeys counts n index keys read; a nil e counts nothing.
func (e *ExaminedCounts) AddKeys(n int) {
	if e != nil {
		e.keys.Add(int64(n))
	}
}

// AddDocs counts n documents read; a nil e counts nothing.
func (e *ExaminedCounts) AddDocs(n int) {
	if e != nil {
		e.docs.Add(int64(n))
	}
}

// AddKeysExamined counts n index keys read under ctx.
func AddKeysExamined(ctx context.Context, n int) {
	ExaminedCountsFrom(ctx).AddKeys(n)
}

// AddBranchKeys counts keys read and entries returned by the branch'th index
// scan under an OR or SORT_MERGE stage; a nil e counts nothing.
func (e *ExaminedCounts) AddBranchKeys(branch int, keys, returned int64) {
	e.updateBranch(branch, func(b *ExplainBranchCounts) {
		b.KeysExamined += keys
		b.NReturned += returned
	})
}

// AddBranchDocs counts the documents the branch'th scan under an OR fetched
// and those matching its filter; a nil e counts nothing.
func (e *ExaminedCounts) AddBranchDocs(branch int, docs, matched int64) {
	e.updateBranch(branch, func(b *ExplainBranchCounts) {
		b.DocsExamined += docs
		b.Matched += matched
	})
}

func (e *ExaminedCounts) updateBranch(branch int, update func(*ExplainBranchCounts)) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.branches) <= branch {
		e.branches = append(e.branches, ExplainBranchCounts{})
	}
	update(&e.branches[branch])
}

// AddDocsExamined counts n documents read under ctx.
func AddDocsExamined(ctx context.Context, n int) {
	ExaminedCountsFrom(ctx).AddDocs(n)
}

type seekEachRangeKey struct{}

// WithSeekEachRange returns a context whose index scans of several ranges
// seek each one, as MongoDB's SBE engine does, examining no key between them.
func WithSeekEachRange(ctx context.Context) context.Context {
	return context.WithValue(ctx, seekEachRangeKey{}, true)
}

// SeeksEachRange reports whether ctx is from WithSeekEachRange.
func SeeksEachRange(ctx context.Context) bool {
	seeks, _ := ctx.Value(seekEachRangeKey{}).(bool)
	return seeks
}

type recordKeysKey struct{}

// WithRecordKeys returns a context whose reads set each document's RecordKey,
// which only showRecordId and $meta "recordId" read; other reads skip it.
func WithRecordKeys(ctx context.Context) context.Context {
	return context.WithValue(ctx, recordKeysKey{}, true)
}

// WantsRecordKeys reports whether ctx is from WithRecordKeys. A read that sets
// keys on many documents resolves it once.
func WantsRecordKeys(ctx context.Context) bool {
	want, _ := ctx.Value(recordKeysKey{}).(bool)
	return want
}
