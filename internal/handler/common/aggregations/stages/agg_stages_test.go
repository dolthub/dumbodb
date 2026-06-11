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

package stages_test

// Tests for multi-stage aggregation pipeline behaviour  -- $group, $unwind, $sort
// ordering and tie-breaking.

import (
	"context"
	"errors"
	"testing"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestAggPipeline_sort_TieBreakingAfterGroup verifies that $sort with multiple keys
// correctly breaks ties using secondary sort fields after a $group stage.
//
// Four groups (A, B, C, D) have counts of 3, 3, 2, 2 respectively.
// Sorting by {count: -1, _id: 1} should produce: A(3), B(3), C(2), D(2)  --
// ties in count are broken by _id ascending.
func TestAggPipeline_sort_TieBreakingAfterGroup(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "A")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "A")),
		must.NotFail(types.NewDocument("_id", int32(3), "tag", "A")),
		must.NotFail(types.NewDocument("_id", int32(4), "tag", "B")),
		must.NotFail(types.NewDocument("_id", int32(5), "tag", "B")),
		must.NotFail(types.NewDocument("_id", int32(6), "tag", "B")),
		must.NotFail(types.NewDocument("_id", int32(7), "tag", "C")),
		must.NotFail(types.NewDocument("_id", int32(8), "tag", "C")),
		must.NotFail(types.NewDocument("_id", int32(9), "tag", "D")),
		must.NotFail(types.NewDocument("_id", int32(10), "tag", "D")),
	}

	// $group: {_id: "$tag", count: {$sum: 1}}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$tag",
		"count", must.NotFail(types.NewDocument("$sum", int32(1))),
	))
	groupDoc := must.NotFail(types.NewDocument("$group", groupSpec))
	groupStage, err := stages.NewStage(groupDoc)
	if err != nil {
		t.Fatalf("NewStage($group): %v", err)
	}

	// $sort: {count: -1, _id: 1}   -- primary by count desc, tiebreak by _id asc
	sortSpec := must.NotFail(types.NewDocument("count", int32(-1), "_id", int32(1)))
	sortDoc := must.NotFail(types.NewDocument("$sort", sortSpec))
	sortStage, err := stages.NewStage(sortDoc)
	if err != nil {
		t.Fatalf("NewStage($sort): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	inputIter := iterator.Values(iterator.ForSlice(docs))

	out, err := groupStage.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("$group Process: %v", err)
	}

	out, err = sortStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$sort Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 4 {
		t.Fatalf("expected 4 groups, got %d", len(results))
	}

	getID := func(doc *types.Document) string {
		v, _ := doc.Get("_id")
		s, _ := v.(string)
		return s
	}

	getCount := func(doc *types.Document) int32 {
		v, _ := doc.Get("count")
		n, _ := v.(int32)
		return n
	}

	// Expected order: A(3), B(3), C(2), D(2)
	// A and B both have count=3  -- sorted by _id asc -> A before B.
	// C and D both have count=2  -- sorted by _id asc -> C before D.
	type want struct {
		id    string
		count int32
	}

	wantOrder := []want{{"A", 3}, {"B", 3}, {"C", 2}, {"D", 2}}

	for i, w := range wantOrder {
		if id := getID(results[i]); id != w.id {
			t.Errorf("results[%d]._id = %q, want %q", i, id, w.id)
		}

		if cnt := getCount(results[i]); cnt != w.count {
			t.Errorf("results[%d].count = %d, want %d", i, cnt, w.count)
		}
	}
}

// TestAggPipeline_multiStage_UnwindThenGroup_tiebreakOrder verifies that a
// $unwind -> $group -> $sort pipeline produces deterministic output when $sort
// keys are tied.
//
// Three documents each carry a two-element "tags" array:
//
//	{_id:1, tags:["a","b"]}, {_id:2, tags:["a","c"]}, {_id:3, tags:["b","d"]}
//
// After $unwind "$tags" the stream is: a, b, a, c, b, d.
// After $group by "$tags" (count=$sum:1): a->2, b->2, c->1, d->1.
// After $sort {count:-1, _id:1}: a(2), b(2) first (tied on count, _id asc keeps
// a before b), then c(1), d(1) (insertion order via stable sort).
func TestAggPipeline_multiStage_UnwindThenGroup_tiebreakOrder(t *testing.T) {
	t.Parallel()

	doc1Tags := types.MakeArray(2)
	doc1Tags.Append("a")
	doc1Tags.Append("b")

	doc2Tags := types.MakeArray(2)
	doc2Tags.Append("a")
	doc2Tags.Append("c")

	doc3Tags := types.MakeArray(2)
	doc3Tags.Append("b")
	doc3Tags.Append("d")

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tags", doc1Tags)),
		must.NotFail(types.NewDocument("_id", int32(2), "tags", doc2Tags)),
		must.NotFail(types.NewDocument("_id", int32(3), "tags", doc3Tags)),
	}

	// $unwind "$tags"
	unwindDoc := must.NotFail(types.NewDocument("$unwind", "$tags"))
	unwindStage, err := stages.NewStage(unwindDoc)
	if err != nil {
		t.Fatalf("NewStage($unwind): %v", err)
	}

	// $group: {_id: "$tags", count: {$sum: 1}}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$tags",
		"count", must.NotFail(types.NewDocument("$sum", int32(1))),
	))
	groupDoc := must.NotFail(types.NewDocument("$group", groupSpec))
	groupStage, err := stages.NewStage(groupDoc)
	if err != nil {
		t.Fatalf("NewStage($group): %v", err)
	}

	// $sort: {count: -1, _id: 1}  -- primary by count desc, tiebreak by _id asc
	sortSpec := must.NotFail(types.NewDocument("count", int32(-1), "_id", int32(1)))
	sortDoc := must.NotFail(types.NewDocument("$sort", sortSpec))
	sortStage, err := stages.NewStage(sortDoc)
	if err != nil {
		t.Fatalf("NewStage($sort): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	inputIter := iterator.Values(iterator.ForSlice(docs))

	out, err := unwindStage.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("$unwind Process: %v", err)
	}

	out, err = groupStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$group Process: %v", err)
	}

	out, err = sortStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$sort Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 4 {
		t.Fatalf("expected 4 groups (a,b,c,d), got %d", len(results))
	}

	getID := func(doc *types.Document) string {
		v, _ := doc.Get("_id")
		s, _ := v.(string)
		return s
	}

	getCount := func(doc *types.Document) int32 {
		v, _ := doc.Get("count")
		n, _ := v.(int32)
		return n
	}

	// Expected: a(2), b(2), c(1), d(1)
	// a and b tied at 2  -- _id asc puts "a" before "b".
	// c and d tied at 1  -- _id asc puts "c" before "d".
	type want struct {
		id    string
		count int32
	}

	wantOrder := []want{{"a", 2}, {"b", 2}, {"c", 1}, {"d", 1}}

	for i, w := range wantOrder {
		if id := getID(results[i]); id != w.id {
			t.Errorf("results[%d]._id = %q, want %q", i, id, w.id)
		}

		if cnt := getCount(results[i]); cnt != w.count {
			t.Errorf("results[%d].count = %d, want %d", i, cnt, w.count)
		}
	}
}

// TestAggStage_sortByCount_TieBreakingOrder verifies that $sortByCount applies an
// implicit secondary sort by _id ascending when counts are equal, per spec:
// $sortByCount === $group + $sort{count:-1, _id:1}.
//
// Six documents produce three groups with counts 2, 2, 2 (all tied).
// Ascending _id tiebreaker must produce: p, q, r.
func TestAggStage_sortByCount_TieBreakingOrder(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "r")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "p")),
		must.NotFail(types.NewDocument("_id", int32(3), "tag", "q")),
		must.NotFail(types.NewDocument("_id", int32(4), "tag", "r")),
		must.NotFail(types.NewDocument("_id", int32(5), "tag", "p")),
		must.NotFail(types.NewDocument("_id", int32(6), "tag", "q")),
	}

	stageDoc := must.NotFail(types.NewDocument("$sortByCount", "$tag"))
	stage, err := stages.NewStage(stageDoc)
	if err != nil {
		t.Fatalf("NewStage($sortByCount): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	out, err := stage.Process(context.Background(), iterator.Values(iterator.ForSlice(docs)), closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 3 {
		t.Fatalf("expected 3 result docs, got %d", len(results))
	}

	// All counts are equal (2). Tiebreaker: _id ascending -> p, q, r.
	wantIDs := []string{"p", "q", "r"}

	for i, wantID := range wantIDs {
		idVal, err := results[i].Get("_id")
		if err != nil {
			t.Errorf("results[%d] missing _id: %v", i, err)
			continue
		}

		if idVal != wantID {
			t.Errorf("results[%d]._id = %v, want %q (ascending _id tiebreaker)", i, idVal, wantID)
		}

		countVal, err := results[i].Get("count")
		if err != nil {
			t.Errorf("results[%d] missing count: %v", i, err)
			continue
		}

		count, ok := countVal.(int32)
		if !ok {
			t.Errorf("results[%d].count is %T, want int32", i, countVal)
			continue
		}

		if count != 2 {
			t.Errorf("results[%d].count = %d, want 2", i, count)
		}
	}
}

// TestAggStage_unsupportedErrors_changeStream verifies that $changeStream returns
// ErrChangeStreamNotSupported (code 40573)  -- standalone servers do not support
// change streams.
func TestAggStage_unsupportedErrors_changeStream(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$changeStream", must.NotFail(types.NewDocument())))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $changeStream, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrChangeStreamNotSupported
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrChangeStreamNotSupported)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_unsupportedErrors_densify verifies that $densify with the required
// 'field' field returns ErrNotImplemented (238)  -- $densify is not yet implemented.
func TestAggStage_unsupportedErrors_densify(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument(
		"field", "price",
		"range", must.NotFail(types.NewDocument("step", int32(1), "bounds", "full")),
	))
	stageDoc := must.NotFail(types.NewDocument("$densify", spec))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $densify, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrNotImplemented
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrNotImplemented)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_unsupportedErrors_fill verifies that $fill with the required 'output'
// field returns ErrNotImplemented (238)  -- $fill is not yet implemented.
func TestAggStage_unsupportedErrors_fill(t *testing.T) {
	t.Parallel()

	spec := must.NotFail(types.NewDocument(
		"output", must.NotFail(types.NewDocument(
			"price", must.NotFail(types.NewDocument("method", "locf")),
		)),
	))
	stageDoc := must.NotFail(types.NewDocument("$fill", spec))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $fill, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrNotImplemented
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrNotImplemented)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_unsupportedErrors_indexStats verifies that $indexStats returns
// ErrStageUnrecognized when called via NewStage, since $indexStats is handled
// specially in the aggregate handler before stage parsing.
func TestAggStage_unsupportedErrors_indexStats(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$indexStats", must.NotFail(types.NewDocument())))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $indexStats via NewStage, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrStageUnrecognized
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrStageUnrecognized)", cmdErr.Code(), wantCode)
	}
}

func TestAggStage_unsupportedErrors_search(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$search", must.NotFail(types.NewDocument(
		"index", "default",
		"text", must.NotFail(types.NewDocument("query", "foo", "path", "bar")),
	))))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $search, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	if got, want := cmdErr.Code(), handlererrors.ErrSearchNotEnabled; got != want {
		t.Errorf("error code = %d, want %d (ErrSearchNotEnabled)", got, want)
	}
	const wantMsg = "$search is not supported by DumboDB."
	if got := cmdErr.Err().Error(); got != wantMsg {
		t.Errorf("error message = %q, want %q", got, wantMsg)
	}
}

func TestAggStage_unsupportedErrors_listSearchIndexes(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$listSearchIndexes", must.NotFail(types.NewDocument())))
	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $listSearchIndexes, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	if got, want := cmdErr.Code(), handlererrors.ErrSearchNotEnabled; got != want {
		t.Errorf("error code = %d, want %d (ErrSearchNotEnabled)", got, want)
	}
	const wantMsg = "$listSearchIndexes is not supported by DumboDB."
	if got := cmdErr.Err().Error(); got != wantMsg {
		t.Errorf("error message = %q, want %q", got, wantMsg)
	}
}

// TestAggStage_limit_LimitZeroError verifies that $limit: 0 returns error code
// 15958 (ErrStageLimitZero) with message "the limit must be positive",
// matching MongoDB 8 behavior.
func TestAggStage_limit_LimitZeroError(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$limit", int32(0)))

	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $limit: 0, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	if got, want := cmdErr.Code(), handlererrors.ErrStageLimitZero; got != want {
		t.Errorf("error code: got %v (%d), want %v (%d)", got, got, want, want)
	}

	if got, want := cmdErr.Err().Error(), "the limit must be positive"; got != want {
		t.Errorf("error message: got %q, want %q", got, want)
	}
}
