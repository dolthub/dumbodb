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

package common

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// trialDocs returns n documents whose field holds [i, i+1], none matching a
// filter on other.
func trialDocs(field string, n int) []*types.Document {
	docs := make([]*types.Document, n)
	for i := range docs {
		docs[i] = must.NotFail(types.NewDocument("_id", int32(i), field, must.NotFail(types.NewArray(int32(i), int32(i+1)))))
	}
	return docs
}

// countingIter yields docs, counting each read into *read.
type countingIter struct {
	docs []*types.Document
	read *int
}

func (it *countingIter) Next() (struct{}, *types.Document, error) {
	if len(it.docs) == 0 {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}
	*it.read++
	doc := it.docs[0]
	it.docs = it.docs[1:]
	return struct{}{}, doc, nil
}

func (it *countingIter) Close() {}

// countingRange returns a rangeIter over docs that counts the documents read
// into *read.
func countingRange(docs []*types.Document, read *int) func(*types.Document) (types.DocumentsIterator, *types.Document, error) {
	return func(*types.Document) (types.DocumentsIterator, *types.Document, error) {
		return &countingIter{docs: docs, read: read}, nil, nil
	}
}

func rangeFilter(fields ...string) *types.Document {
	f := must.NotFail(types.NewDocument("nomatch", true))
	for _, field := range fields {
		f.Set(field, must.NotFail(types.NewDocument("$gt", int32(10), "$lt", int32(20))))
	}
	return f
}

// TestBoundedByTrialSharesOneBudget: the trials of every multikey field of
// one query share one budget, so the documents they read stay within it.
func TestBoundedByTrialSharesOneBudget(t *testing.T) {
	const fields = 32
	budget := &TrialBudget{PlanWorks: 1000, Works: 2000, NumToReturn: TrialMaxAdvanced}
	names := make([]string, fields)
	for i := range names {
		names[i] = fmt.Sprintf("f%d", i)
	}
	filter := rangeFilter(names...)
	read := 0
	for _, name := range names {
		var err error
		if filter, err = BoundedByTrial(context.Background(), filter, name, countingRange(trialDocs(name, 10000), &read), budget, nil); err != nil {
			t.Fatal(err)
		}
	}
	if read > 2000 {
		t.Fatalf("trials read %d documents, want at most the budget's 2000", read)
	}
}

// TestBoundedByTrialStopsOnCancel: a cancelled context stops the trial.
func TestBoundedByTrialStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read := 0
	_, err := BoundedByTrial(ctx, rangeFilter("a"), "a", countingRange(trialDocs("a", 10000), &read),
		&TrialBudget{PlanWorks: 1 << 20, Works: 1 << 21, NumToReturn: TrialMaxAdvanced}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if read != 0 {
		t.Fatalf("read %d documents after cancellation", read)
	}
}

// TestBoundedByTrialKeepsEmptyBound: a bound whose scan ends at once is kept,
// and the other scan stops right after it.
func TestBoundedByTrialKeepsEmptyBound(t *testing.T) {
	docs := trialDocs("a", 10000)
	read := 0
	rangeIter := func(cond *types.Document) (types.DocumentsIterator, *types.Document, error) {
		if cond.Has("$gt") {
			return countingRange(nil, &read)(cond)
		}
		return countingRange(docs, &read)(cond)
	}
	got, err := BoundedByTrial(context.Background(), rangeFilter("a"), "a", rangeIter,
		&TrialBudget{PlanWorks: 1 << 20, Works: 1 << 21, NumToReturn: TrialMaxAdvanced}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !must.NotFail(got.Get("a")).(*types.Document).Has("$gt") {
		t.Fatalf("kept %v, want the empty lower bound", got)
	}
	if read > 2 {
		t.Fatalf("read %d documents, want the trial to stop once the empty scan ends", read)
	}
}
