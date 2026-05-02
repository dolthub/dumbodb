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

// Tests for complex $lookup and $graphLookup scenarios:
//   - Pipeline form with let variables (correlated subquery)
//   - Array localField (unwind-join pattern)
//   - Nested $lookup (lookup within a lookup pipeline)
//   - $graphLookup: recursive graph traversal with startWith, connectFromField,
//     connectToField, as, maxDepth, depthField, and restrictSearchWithMatch

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

// makeFetcher builds a CollectionFetcher that returns docs for a named collection.
func makeFetcher(collections map[string][]*types.Document) stages.CollectionFetcher {
	return func(_ context.Context, name string) ([]*types.Document, error) {
		return collections[name], nil
	}
}

// collectResults drains an iterator into a slice.
func collectResults(t *testing.T, iter types.DocumentsIterator, closer *iterator.MultiCloser) []*types.Document {
	t.Helper()

	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		t.Fatalf("ConsumeValues: %v", err)
	}

	return docs
}

// TestLookup_SimpleEqualityJoin verifies the basic localField/foreignField form.
func TestLookup_SimpleEqualityJoin(t *testing.T) {
	t.Parallel()

	orders := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(10), "custId", int32(1), "item", "apple")),
		must.NotFail(types.NewDocument("_id", int32(11), "custId", int32(2), "item", "banana")),
		must.NotFail(types.NewDocument("_id", int32(12), "custId", int32(1), "item", "cherry")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"orders": orders})

	spec := must.NotFail(types.NewDocument(
		"from", "orders",
		"localField", "_id",
		"foreignField", "custId",
		"as", "myOrders",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	customers := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "Alice")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "Bob")),
	}

	inputIter := iterator.Values(iterator.ForSlice(customers))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 2 {
		t.Fatalf("expected 2 output docs, got %d", len(results))
	}

	// Alice should have 2 orders.
	aliceOrders, err := results[0].Get("myOrders")
	if err != nil {
		t.Fatalf("Alice missing myOrders: %v", err)
	}

	aliceArr, ok := aliceOrders.(*types.Array)
	if !ok {
		t.Fatalf("myOrders not an array: %T", aliceOrders)
	}

	if aliceArr.Len() != 2 {
		t.Errorf("Alice: expected 2 orders, got %d", aliceArr.Len())
	}

	// Bob should have 1 order.
	bobOrders, err := results[1].Get("myOrders")
	if err != nil {
		t.Fatalf("Bob missing myOrders: %v", err)
	}

	bobArr, ok := bobOrders.(*types.Array)
	if !ok {
		t.Fatalf("myOrders not an array: %T", bobOrders)
	}

	if bobArr.Len() != 1 {
		t.Errorf("Bob: expected 1 order, got %d", bobArr.Len())
	}
}

// TestLookup_PipelineFormNoLet verifies pipeline form without let variables
// returns all from-collection documents (uncorrelated subpipeline).
func TestLookup_PipelineFormNoLet(t *testing.T) {
	t.Parallel()

	tags := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "go")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "rust")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"tags": tags})

	pipeline := must.NotFail(types.NewArray())

	spec := must.NotFail(types.NewDocument(
		"from", "tags",
		"pipeline", pipeline,
		"as", "allTags",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(100), "name", "article"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	allTags, err := results[0].Get("allTags")
	if err != nil {
		t.Fatalf("missing allTags: %v", err)
	}

	tagsArr, ok := allTags.(*types.Array)
	if !ok {
		t.Fatalf("allTags not an array: %T", allTags)
	}

	if tagsArr.Len() != 2 {
		t.Errorf("expected 2 tags (all from-coll docs), got %d", tagsArr.Len())
	}
}

// TestLookup_PipelineFormWithLet verifies pipeline form with let variables.
// Let binds a field from the local document; the subpipeline $match uses $$var
// to filter the from collection to only matching docs.
//
// After variable substitution, the $$cid reference becomes the scalar value
// from the local document, so the match filter resolves to {custId: <value>}.
func TestLookup_PipelineFormWithLet(t *testing.T) {
	t.Parallel()

	// orders collection: each doc has a custId field.
	orders := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(10), "custId", int32(1), "amount", int32(100))),
		must.NotFail(types.NewDocument("_id", int32(11), "custId", int32(2), "amount", int32(200))),
		must.NotFail(types.NewDocument("_id", int32(12), "custId", int32(1), "amount", int32(150))),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"orders": orders})

	// Pipeline: [{$match: {custId: "$$cid"}}]
	// After substitution, $$cid becomes the local _id value (e.g. int32(1)),
	// producing {$match: {custId: int32(1)}} which uses the existing equality filter.
	matchFilter := must.NotFail(types.NewDocument("custId", "$$cid"))
	matchStage := must.NotFail(types.NewDocument("$match", matchFilter))

	pipeline := must.NotFail(types.NewArray(matchStage))
	letDoc := must.NotFail(types.NewDocument("cid", "$_id"))

	spec := must.NotFail(types.NewDocument(
		"from", "orders",
		"let", letDoc,
		"pipeline", pipeline,
		"as", "custOrders",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	customers := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "Alice")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "Bob")),
	}

	inputIter := iterator.Values(iterator.ForSlice(customers))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 2 {
		t.Fatalf("expected 2 output docs, got %d", len(results))
	}

	// Alice (_id=1) should have orders 10 and 12.
	aliceOrders, err := results[0].Get("custOrders")
	if err != nil {
		t.Fatalf("Alice missing custOrders: %v", err)
	}

	aliceArr, ok := aliceOrders.(*types.Array)
	if !ok {
		t.Fatalf("custOrders not an array: %T", aliceOrders)
	}

	if aliceArr.Len() != 2 {
		t.Errorf("Alice: expected 2 matching orders, got %d", aliceArr.Len())
	}

	// Bob (_id=2) should have order 11 only.
	bobOrders, err := results[1].Get("custOrders")
	if err != nil {
		t.Fatalf("Bob missing custOrders: %v", err)
	}

	bobArr, ok := bobOrders.(*types.Array)
	if !ok {
		t.Fatalf("custOrders not an array: %T", bobOrders)
	}

	if bobArr.Len() != 1 {
		t.Errorf("Bob: expected 1 matching order, got %d", bobArr.Len())
	}
}

// TestLookup_ArrayLocalField verifies that when localField holds an array,
// documents whose foreignField value appears in the array are matched.
func TestLookup_ArrayLocalField(t *testing.T) {
	t.Parallel()

	// items collection with tags field as an array.
	items := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "color", "red")),
		must.NotFail(types.NewDocument("_id", int32(2), "color", "blue")),
		must.NotFail(types.NewDocument("_id", int32(3), "color", "green")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"items": items})

	spec := must.NotFail(types.NewDocument(
		"from", "items",
		"localField", "colorRefs",
		"foreignField", "color",
		"as", "matched",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	// Local doc has colorRefs: ["red", "green"]  -- should match items 1 and 3.
	colorRefs := must.NotFail(types.NewArray("red", "green"))
	localDoc := must.NotFail(types.NewDocument("_id", int32(100), "colorRefs", colorRefs))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{localDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	matched, err := results[0].Get("matched")
	if err != nil {
		t.Fatalf("missing matched: %v", err)
	}

	matchedArr, ok := matched.(*types.Array)
	if !ok {
		t.Fatalf("matched not an array: %T", matched)
	}

	if matchedArr.Len() != 2 {
		t.Errorf("expected 2 matched items (red and green), got %d", matchedArr.Len())
	}
}

// TestLookup_NestedLookup verifies that a $lookup stage inside the subpipeline works.
// This tests nested $lookup: the outer lookup's pipeline contains another $lookup.
func TestLookup_NestedLookup(t *testing.T) {
	t.Parallel()

	// details collection: detail docs linked to orders.
	details := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(100), "orderId", int32(10), "note", "fragile")),
		must.NotFail(types.NewDocument("_id", int32(101), "orderId", int32(11), "note", "urgent")),
	}

	// orders collection: order docs that will be joined with details.
	orders := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(10), "custId", int32(1), "item", "vase")),
		must.NotFail(types.NewDocument("_id", int32(11), "custId", int32(2), "item", "book")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{
		"orders":  orders,
		"details": details,
	})

	// Inner $lookup: join orders with details on orders._id = details.orderId.
	innerLookupSpec := must.NotFail(types.NewDocument(
		"from", "details",
		"localField", "_id",
		"foreignField", "orderId",
		"as", "orderDetails",
	))
	innerLookupStage := must.NotFail(types.NewDocument("$lookup", innerLookupSpec))

	// Outer pipeline contains the inner $lookup.
	outerPipeline := must.NotFail(types.NewArray(innerLookupStage))

	// Outer $lookup: join customers with orders via pipeline.
	// No let needed  -- this is an uncorrelated sub-pipeline that fetches all orders
	// and enriches them with their details via nested $lookup.
	outerSpec := must.NotFail(types.NewDocument(
		"from", "orders",
		"pipeline", outerPipeline,
		"as", "enrichedOrders",
	))
	outerStageDoc := must.NotFail(types.NewDocument("$lookup", outerSpec))

	s, err := stages.NewLookupStage(outerStageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage (outer): %v", err)
	}

	customer := must.NotFail(types.NewDocument("_id", int32(1), "name", "Alice"))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{customer}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	enrichedOrders, err := results[0].Get("enrichedOrders")
	if err != nil {
		t.Fatalf("missing enrichedOrders: %v", err)
	}

	enrichedArr, ok := enrichedOrders.(*types.Array)
	if !ok {
		t.Fatalf("enrichedOrders not an array: %T", enrichedOrders)
	}

	// Should have both orders (all from orders collection since no filter).
	if enrichedArr.Len() != 2 {
		t.Errorf("expected 2 enriched orders, got %d", enrichedArr.Len())
	}

	// Each order should have an orderDetails array with 1 detail.
	for i := 0; i < enrichedArr.Len(); i++ {
		v, _ := enrichedArr.Get(i)
		orderDoc, ok := v.(*types.Document)
		if !ok {
			t.Errorf("enrichedOrders[%d] is not a document: %T", i, v)
			continue
		}

		detailsVal, err := orderDoc.Get("orderDetails")
		if err != nil {
			t.Errorf("enrichedOrders[%d] missing orderDetails: %v", i, err)
			continue
		}

		detailsArr, ok := detailsVal.(*types.Array)
		if !ok {
			t.Errorf("orderDetails is not an array: %T", detailsVal)
			continue
		}

		if detailsArr.Len() != 1 {
			t.Errorf("enrichedOrders[%d]: expected 1 detail, got %d", i, detailsArr.Len())
		}
	}
}

// TestLookup_LetVariableNotInLocalDoc verifies that a let variable referencing
// a missing field resolves to null and does not cause an error.
//
// When the let expression "$nonExistentField" is evaluated against a doc that
// lacks that field, the variable resolves to Null. The subsequent $match for
// {custId: Null} will not match any order with a non-null custId.
func TestLookup_LetVariableNotInLocalDoc(t *testing.T) {
	t.Parallel()

	orders := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(10), "custId", int32(1))),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"orders": orders})

	// $$missing resolves to Null; {custId: Null} won't match custId: 1.
	matchFilter := must.NotFail(types.NewDocument("custId", "$$missing"))
	matchStage := must.NotFail(types.NewDocument("$match", matchFilter))

	pipeline := must.NotFail(types.NewArray(matchStage))
	letDoc := must.NotFail(types.NewDocument("missing", "$nonExistentField"))

	spec := must.NotFail(types.NewDocument(
		"from", "orders",
		"let", letDoc,
		"pipeline", pipeline,
		"as", "result",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	localDoc := must.NotFail(types.NewDocument("_id", int32(99), "name", "Ghost"))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{localDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	// The result array should be empty since null != 1.
	resultVal, err := results[0].Get("result")
	if err != nil {
		t.Fatalf("missing result: %v", err)
	}

	resultArr, ok := resultVal.(*types.Array)
	if !ok {
		t.Fatalf("result not an array: %T", resultVal)
	}

	if resultArr.Len() != 0 {
		t.Errorf("expected 0 matched docs (null != custId values), got %d", resultArr.Len())
	}
}

// TestLookup_ArrayLocalFieldNoMatch verifies that when the array localField
// has no intersection with foreign values, an empty array is produced.
func TestLookup_ArrayLocalFieldNoMatch(t *testing.T) {
	t.Parallel()

	items := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "color", "red")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"items": items})

	spec := must.NotFail(types.NewDocument(
		"from", "items",
		"localField", "colorRefs",
		"foreignField", "color",
		"as", "matched",
	))
	stageDoc := must.NotFail(types.NewDocument("$lookup", spec))

	s, err := stages.NewLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewLookupStage: %v", err)
	}

	// colorRefs: ["blue", "green"]  -- no intersection with items collection.
	colorRefs := must.NotFail(types.NewArray("blue", "green"))
	localDoc := must.NotFail(types.NewDocument("_id", int32(100), "colorRefs", colorRefs))

	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{localDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	matched, err := results[0].Get("matched")
	if err != nil {
		t.Fatalf("missing matched: %v", err)
	}

	matchedArr, ok := matched.(*types.Array)
	if !ok {
		t.Fatalf("matched not an array: %T", matched)
	}

	if matchedArr.Len() != 0 {
		t.Errorf("expected 0 matches, got %d", matchedArr.Len())
	}
}

// TestGraphLookup_BasicTraversal verifies simple recursive graph traversal.
//
// Graph: alice -> bob -> carol  (each employee's reportsTo field points to their manager's name).
// Starting from alice, we expect to find bob and carol in the result.
func TestGraphLookup_BasicTraversal(t *testing.T) {
	t.Parallel()

	// employees collection: reportsTo links one employee to another by name.
	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "bob", "reportsTo", "carol")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "carol")), // top of hierarchy
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	// Starting from alice, we follow: reportsTo=bob (depth 0), then bob.reportsTo=carol (depth 1).
	if managers.Len() != 2 {
		t.Errorf("expected 2 managers (bob and carol), got %d", managers.Len())
	}
}

// TestGraphLookup_MaxDepth verifies that maxDepth limits the traversal depth.
//
// With maxDepth: 0 only the immediate managers are returned (no further traversal).
func TestGraphLookup_MaxDepth(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "bob", "reportsTo", "carol")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "carol")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
		"maxDepth", int32(0),
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	// maxDepth 0: only depth-0 documents (bob) are returned; carol is not visited.
	if managers.Len() != 1 {
		t.Errorf("expected 1 manager (bob only, maxDepth=0), got %d", managers.Len())
	}

	v, _ := managers.Get(0)
	doc, ok := v.(*types.Document)
	if !ok {
		t.Fatalf("element not a document: %T", v)
	}

	name, err := doc.Get("name")
	if err != nil || name != "bob" {
		t.Errorf("expected name=bob, got %v (err=%v)", name, err)
	}
}

// TestGraphLookup_DepthField verifies that depthField is added to each result document.
func TestGraphLookup_DepthField(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "bob", "reportsTo", "carol")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "carol")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
		"depthField", "depth",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	if managers.Len() != 2 {
		t.Fatalf("expected 2 managers, got %d", managers.Len())
	}

	// Verify depthField is set correctly.
	for i := 0; i < managers.Len(); i++ {
		v, _ := managers.Get(i)
		mgr, ok := v.(*types.Document)
		if !ok {
			t.Errorf("managers[%d] not a document: %T", i, v)
			continue
		}

		depthVal, dErr := mgr.Get("depth")
		if dErr != nil {
			t.Errorf("managers[%d] missing 'depth' field: %v", i, dErr)
			continue
		}

		// Depth should be 0 for bob (first hop) and 1 for carol (second hop).
		// MongoDB returns depthField as int32 for small values.
		depth, ok := depthVal.(int32)
		if !ok {
			t.Errorf("managers[%d].depth not int32: %T (%v)", i, depthVal, depthVal)
		} else if depth < 0 || depth > 1 {
			t.Errorf("managers[%d].depth out of expected range [0,1]: %d", i, depth)
		}
	}
}

// TestGraphLookup_RestrictSearchWithMatch verifies that restrictSearchWithMatch
// filters out documents that do not match the given condition.
func TestGraphLookup_RestrictSearchWithMatch(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob", "active", true)),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "bob", "reportsTo", "carol", "active", false)),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "carol", "active", true)),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	// Only follow edges through active employees.
	restrictFilter := must.NotFail(types.NewDocument("active", true))

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
		"restrictSearchWithMatch", restrictFilter,
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	// Alice (reportsTo=bob). bob is inactive, so traversal stops at depth 0.
	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob", "active", true))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	// bob is filtered out (active=false), so no results.
	if managers.Len() != 0 {
		t.Errorf("expected 0 managers (bob filtered by restrictSearchWithMatch), got %d", managers.Len())
	}
}

// TestGraphLookup_CycleDetection verifies that cyclic graphs do not cause infinite loops.
//
// Graph: a <-> b (a.reportsTo=b, b.reportsTo=a). The traversal should terminate
// after visiting each node once.
func TestGraphLookup_CycleDetection(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "a", "reportsTo", "b")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "b", "reportsTo", "a")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "related",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	// Start from employee "a" (reportsTo=b).
	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "a", "reportsTo", "b"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	relatedVal, err := results[0].Get("related")
	if err != nil {
		t.Fatalf("missing related: %v", err)
	}

	related, ok := relatedVal.(*types.Array)
	if !ok {
		t.Fatalf("related not an array: %T", relatedVal)
	}

	// Should find b (from reportsTo=b), then attempt a (from b.reportsTo=a) but
	// "a" was the search value that started it all  -- actually "b" was searched first,
	// then "a" is queued. "a" has not been searched yet, so we look for connectToField=a
	// and find employee "a". But then a.reportsTo=b which was already searched, so we stop.
	// Result: b and a -> 2 documents.
	if related.Len() != 2 {
		t.Errorf("expected 2 related docs (a and b, cycle terminates), got %d", related.Len())
	}
}

// TestGraphLookup_StartWithMissingField verifies that a missing startWith field
// produces an empty result array (no traversal begins).
func TestGraphLookup_StartWithMissingField(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$nonExistentField",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(99), "name", "ghost"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	if managers.Len() != 0 {
		t.Errorf("expected 0 managers (no startWith value), got %d", managers.Len())
	}
}

// TestAggComplex_matchGroupProject_addToSet verifies that a pipeline of
// $match -> $group (with $addToSet) -> $project preserves encounter order,
// matching MongoDB's behavior where $addToSet retains first-seen order.
func TestAggComplex_matchGroupProject_addToSet(t *testing.T) {
	t.Parallel()

	// Three orders with two distinct statuses: "pending" appears first in the
	// slice, "cancelled" second. $addToSet preserves encounter order:
	// "pending" is seen first and "cancelled" second.
	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "customerId", int32(100), "status", "pending")),
		must.NotFail(types.NewDocument("_id", int32(2), "customerId", int32(100), "status", "cancelled")),
		must.NotFail(types.NewDocument("_id", int32(3), "customerId", int32(100), "status", "pending")),
	}

	// $match: {customerId: 100}
	matchDoc := must.NotFail(types.NewDocument(
		"$match", must.NotFail(types.NewDocument("customerId", int32(100))),
	))
	matchStage, err := stages.NewStage(matchDoc)
	if err != nil {
		t.Fatalf("NewStage($match): %v", err)
	}

	// $group: {_id: "$customerId", uniqueStatuses: {$addToSet: "$status"}}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$customerId",
		"uniqueStatuses", must.NotFail(types.NewDocument("$addToSet", "$status")),
	))
	groupDoc := must.NotFail(types.NewDocument("$group", groupSpec))
	groupStage, err := stages.NewStage(groupDoc)
	if err != nil {
		t.Fatalf("NewStage($group): %v", err)
	}

	// $project: {_id: 0, uniqueStatuses: 1}
	projectSpec := must.NotFail(types.NewDocument("_id", int32(0), "uniqueStatuses", int32(1)))
	projectDoc := must.NotFail(types.NewDocument("$project", projectSpec))
	projectStage, err := stages.NewStage(projectDoc)
	if err != nil {
		t.Fatalf("NewStage($project): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	inputIter := iterator.Values(iterator.ForSlice(docs))

	out, err := matchStage.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("$match Process: %v", err)
	}

	out, err = groupStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$group Process: %v", err)
	}

	out, err = projectStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$project Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	rawStatuses, err := results[0].Get("uniqueStatuses")
	if err != nil {
		t.Fatalf("missing uniqueStatuses: %v", err)
	}

	statuses, ok := rawStatuses.(*types.Array)
	if !ok {
		t.Fatalf("uniqueStatuses is not an array: %T", rawStatuses)
	}

	if statuses.Len() != 2 {
		t.Fatalf("expected 2 unique statuses, got %d", statuses.Len())
	}

	// MongoDB preserves encounter order; "pending" is seen before "cancelled".
	s0, _ := statuses.Get(0)
	s1, _ := statuses.Get(1)

	if s0 != "pending" || s1 != "cancelled" {
		t.Errorf("expected [pending cancelled], got [%v %v]", s0, s1)
	}
}

// TestAggComplex_replaceRoot_mergeObjects verifies that $replaceRoot with a
// $mergeObjects newRoot expression correctly merges $$ROOT with a literal
// document. Previously, dumbodb treated {$mergeObjects: ...} as a literal
// template and returned it verbatim, causing wrong results (and a server crash
// in some configurations).
//
// Regression test for do-e4pc.
func TestAggComplex_replaceRoot_mergeObjects(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "Alice", "score", int32(90))),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "Bob", "score", int32(75))),
	}

	// {$replaceRoot: {newRoot: {$mergeObjects: ["$$ROOT", {grade: "A"}]}}}
	mergeSpec := must.NotFail(types.NewDocument(
		"$mergeObjects", must.NotFail(types.NewArray(
			"$$ROOT",
			must.NotFail(types.NewDocument("grade", "A")),
		)),
	))
	stageDoc := must.NotFail(types.NewDocument(
		"$replaceRoot", must.NotFail(types.NewDocument("newRoot", mergeSpec)),
	))

	stage, err := stages.NewStage(stageDoc)
	if err != nil {
		t.Fatalf("NewStage($replaceRoot): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	inputIter := iterator.Values(iterator.ForSlice(docs))

	out, err := stage.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(results))
	}

	// Each result doc must have the original fields plus grade="A".
	for i, res := range results {
		name, nameErr := res.Get("name")
		score, scoreErr := res.Get("score")
		grade, gradeErr := res.Get("grade")

		if nameErr != nil || scoreErr != nil || gradeErr != nil {
			t.Errorf("results[%d] missing expected fields: name=%v score=%v grade=%v", i, nameErr, scoreErr, gradeErr)
			continue
		}

		if grade != "A" {
			t.Errorf("results[%d]: expected grade=A, got %v", i, grade)
		}

		_ = name
		_ = score
	}

	// Verify field values for first doc.
	name0, _ := results[0].Get("name")
	if name0 != "Alice" {
		t.Errorf("results[0].name: expected Alice, got %v", name0)
	}
}

// TestAggComplex_mergeObjects_laterFieldsWin verifies that when two documents
// passed to $mergeObjects share a key, the later document's value wins.
func TestAggComplex_mergeObjects_laterFieldsWin(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "status", "pending", "category", "X")),
	}

	// {$replaceRoot: {newRoot: {$mergeObjects: ["$$ROOT", {status: "active"}]}}}
	// "status" in $$ROOT is "pending"; the second doc overrides it with "active".
	mergeSpec := must.NotFail(types.NewDocument(
		"$mergeObjects", must.NotFail(types.NewArray(
			"$$ROOT",
			must.NotFail(types.NewDocument("status", "active")),
		)),
	))
	stageDoc := must.NotFail(types.NewDocument(
		"$replaceRoot", must.NotFail(types.NewDocument("newRoot", mergeSpec)),
	))

	stage, err := stages.NewStage(stageDoc)
	if err != nil {
		t.Fatalf("NewStage: %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	out, err := stage.Process(context.Background(), iterator.Values(iterator.ForSlice(docs)), closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, out, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(results))
	}

	status, _ := results[0].Get("status")
	category, _ := results[0].Get("category")

	if status != "active" {
		t.Errorf("expected status=active (later doc wins), got %v", status)
	}

	if category != "X" {
		t.Errorf("expected category=X (from $$ROOT), got %v", category)
	}
}

// TestAggComplex_matchUnwindGroupSort verifies that $sort is stable: when two
// groups have the same sort key value, they retain the order produced by the
// preceding pipeline stage (matching MongoDB behavior).
//
// Regression test for do-socd: $sort tie-breaking in sort+group pipeline.
func TestAggComplex_matchUnwindGroupSort(t *testing.T) {
	t.Parallel()

	// Three orders across two customers. After $unwind+$group by category,
	// groups B and A both have totalQty=2. MongoDB preserves insertion order
	// for ties; DumboDB must do the same (stable sort).
	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "category", "C", "qty", int32(8))),
		must.NotFail(types.NewDocument("_id", int32(2), "category", "B", "qty", int32(2))),
		must.NotFail(types.NewDocument("_id", int32(3), "category", "A", "qty", int32(2))),
	}

	// $group: {_id: "$category", orderCount: {$sum: 1}, totalQty: {$sum: "$qty"}}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$category",
		"orderCount", must.NotFail(types.NewDocument("$sum", int32(1))),
		"totalQty", must.NotFail(types.NewDocument("$sum", "$qty")),
	))
	groupDoc := must.NotFail(types.NewDocument("$group", groupSpec))
	groupStage, err := stages.NewStage(groupDoc)
	if err != nil {
		t.Fatalf("NewStage($group): %v", err)
	}

	// $sort: {totalQty: -1}
	sortDoc := must.NotFail(types.NewDocument("$sort", must.NotFail(types.NewDocument("totalQty", int32(-1)))))
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

	if len(results) != 3 {
		t.Fatalf("expected 3 output docs, got %d", len(results))
	}

	// First doc must be C (totalQty=8). B and A both have totalQty=2; because
	// the sort is stable, B (inserted before A) must appear before A.
	getID := func(doc *types.Document) string {
		v, _ := doc.Get("_id")
		s, _ := v.(string)
		return s
	}

	if id := getID(results[0]); id != "C" {
		t.Errorf("results[0]: expected _id=C, got %q", id)
	}
	if id := getID(results[1]); id != "B" {
		t.Errorf("results[1]: expected _id=B (stable tie-break), got %q", id)
	}
	if id := getID(results[2]); id != "A" {
		t.Errorf("results[2]: expected _id=A (stable tie-break), got %q", id)
	}
}

// TestAggStage_graphLookup_MaxDepthLimitsTraversal verifies that maxDepth correctly
// limits traversal and that depthField values are returned as int32 (matching MongoDB).
//
// Graph: alice -> bob -> carol -> dave -> eve (5-node chain)
// With maxDepth=2, only bob (depth 0), carol (depth 1), and dave (depth 2) should
// appear. eve at depth 3 must NOT be included.
func TestAggStage_graphLookup_MaxDepthLimitsTraversal(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "bob", "reportsTo", "carol")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "carol", "reportsTo", "dave")),
		must.NotFail(types.NewDocument("_id", int32(4), "name", "dave", "reportsTo", "eve")),
		must.NotFail(types.NewDocument("_id", int32(5), "name", "eve")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "managers",
		"maxDepth", int32(2),
		"depthField", "depth",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(1), "name", "alice", "reportsTo", "bob"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	managersVal, err := results[0].Get("managers")
	if err != nil {
		t.Fatalf("missing managers: %v", err)
	}

	managers, ok := managersVal.(*types.Array)
	if !ok {
		t.Fatalf("managers not an array: %T", managersVal)
	}

	// maxDepth=2: bob (depth 0), carol (depth 1), dave (depth 2). eve is excluded.
	if managers.Len() != 3 {
		t.Fatalf("expected 3 managers (bob, carol, dave), got %d", managers.Len())
	}

	// MongoDB ordering: depth-0 first, then remaining levels deepest-first.
	// For this linear chain: bob(d0), dave(d2), carol(d1).
	// Verify depthField is int32 (matching MongoDB BSON wire type).
	expectedNames := []string{"bob", "dave", "carol"}
	expectedDepths := []int32{0, 2, 1}

	for i := 0; i < managers.Len(); i++ {
		v, _ := managers.Get(i)
		mgr, ok := v.(*types.Document)
		if !ok {
			t.Errorf("managers[%d] not a document: %T", i, v)
			continue
		}

		nameVal, err := mgr.Get("name")
		if err != nil {
			t.Errorf("managers[%d] missing name: %v", i, err)
			continue
		}

		if nameVal != expectedNames[i] {
			t.Errorf("managers[%d].name = %v, want %v", i, nameVal, expectedNames[i])
		}

		depthVal, err := mgr.Get("depth")
		if err != nil {
			t.Errorf("managers[%d] missing depth: %v", i, err)
			continue
		}

		// depthField must be int32, matching MongoDB's BSON wire representation.
		depth, ok := depthVal.(int32)
		if !ok {
			t.Errorf("managers[%d].depth is %T, want int32", i, depthVal)
			continue
		}

		if depth != expectedDepths[i] {
			t.Errorf("managers[%d].depth = %d, want %d", i, depth, expectedDepths[i])
		}
	}
}

// TestAggStage_bucket_MissingBoundariesError verifies that $bucket returns error
// code 40198 when the required 'boundaries' field is absent.
func TestAggStage_bucket_MissingBoundariesError(t *testing.T) {
	t.Parallel()

	// Build a $bucket spec with groupBy but no boundaries field.
	spec := must.NotFail(types.NewDocument("groupBy", "$x"))
	stageDoc := must.NotFail(types.NewDocument("$bucket", spec))

	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for missing boundaries, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrBucketMissingBoundaries
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrBucketMissingBoundaries)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_bucket_OneBoundaryError verifies that $bucket returns error code 40192
// when the 'boundaries' array has fewer than 2 values (here: exactly 1).
// MongoDB requires at least 2 boundaries to define at least one bucket.
func TestAggStage_bucket_OneBoundaryError(t *testing.T) {
	t.Parallel()

	bounds := types.MakeArray(1)
	bounds.Append(int32(0))

	spec := must.NotFail(types.NewDocument("groupBy", "$x", "boundaries", bounds))
	stageDoc := must.NotFail(types.NewDocument("$bucket", spec))

	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for one boundary, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrBucketNotEnoughBoundaries
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrBucketNotEnoughBoundaries)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_bucketAuto_MissingBucketsError verifies that $bucketAuto returns error
// code 40246 when the required 'buckets' field is absent.
func TestAggStage_bucketAuto_MissingBucketsError(t *testing.T) {
	t.Parallel()

	// Build a $bucketAuto spec with groupBy but no buckets field.
	spec := must.NotFail(types.NewDocument("groupBy", "$x"))
	stageDoc := must.NotFail(types.NewDocument("$bucketAuto", spec))

	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for missing buckets, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	const wantCode = handlererrors.ErrBucketAutoMissingRequiredFields
	if cmdErr.Code() != wantCode {
		t.Errorf("error code = %d, want %d (ErrBucketAutoMissingRequiredFields)", cmdErr.Code(), wantCode)
	}
}

// TestAggStage_graphLookup_DeterministicOrdering verifies that $graphLookup returns
// results in a deterministic order even when multiple documents match the same BFS
// frontier value (i.e., multiple nodes at the same depth level).
//
// Graph: ceo has two direct reports, vp (_id=1) and mgr (_id=2), both with
// reportsTo="ceo". Starting with "ceo" as the search value, both vp and mgr match
// in the same BFS pass. Their order must be by _id ascending regardless of the
// collection scan order returned by the storage backend.
//
// This is a regression test for the intermittent failure where DumboDB returned
// [{mgr,...},{vp,...}] instead of [{vp,...},{mgr,...}] depending on the run.
func TestAggStage_graphLookup_DeterministicOrdering(t *testing.T) {
	t.Parallel()

	// Two subordinates both report to "ceo"  -- they match in the same BFS pass.
	// vp has _id=1, mgr has _id=2; expected result order is vp first (lower _id).
	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "vp", "reportsTo", "ceo")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "mgr", "reportsTo", "ceo")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "ceo")),
	}

	// Intentionally shuffle the fetcher order to simulate a non-deterministic
	// storage scan that returns mgr before vp.
	shuffled := []*types.Document{employees[1], employees[2], employees[0]} // mgr, ceo, vp
	fetcher := makeFetcher(map[string][]*types.Document{"employees": shuffled})

	// Traverse downward: starting from "ceo", find all employees whose reportsTo="ceo",
	// then enqueue their names as the next search tier.
	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "ceo",          // literal: start the BFS at "ceo"
		"connectFromField", "name",   // enqueue the name of each found doc
		"connectToField", "reportsTo", // match docs where reportsTo = queued value
		"as", "subordinates",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	inputDoc := must.NotFail(types.NewDocument("_id", int32(4), "name", "alice"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	subVal, err := results[0].Get("subordinates")
	if err != nil {
		t.Fatalf("missing subordinates: %v", err)
	}

	subs, ok := subVal.(*types.Array)
	if !ok {
		t.Fatalf("subordinates not an array: %T", subVal)
	}

	// Both vp (_id=1) and mgr (_id=2) have reportsTo="ceo", so both match in the
	// first BFS pass. After sorting fromDocs by _id, vp must appear before mgr.
	if subs.Len() != 2 {
		t.Fatalf("expected 2 subordinates (vp and mgr), got %d", subs.Len())
	}

	expectedNames := []string{"vp", "mgr"}

	for i := 0; i < subs.Len(); i++ {
		v, _ := subs.Get(i)
		sub, ok := v.(*types.Document)
		if !ok {
			t.Errorf("subordinates[%d] not a document: %T", i, v)
			continue
		}

		nameVal, err := sub.Get("name")
		if err != nil {
			t.Errorf("subordinates[%d] missing name: %v", i, err)
			continue
		}

		if nameVal != expectedNames[i] {
			t.Errorf("subordinates[%d].name = %v, want %v", i, nameVal, expectedNames[i])
		}
	}
}

// TestAggStage_graphLookup_TraverseHierarchyFromLeaf is a regression test for the
// within-level ordering bug where $graphLookup returned results in frontier-value order
// rather than collection-scan (_id) order.
//
// Graph (upward reporting chain from a leaf):
//
//	jr -> mgr -> [vp, ceo]   (mgr.reportsTo is an array ["vp","ceo"])
//	ceo and vp are both terminal (no further reportsTo)
//
// Starting from jr (startWith = "$reportsTo" = "mgr"), the BFS discovers:
//   - depth 0: mgr   (name matches "mgr")
//   - depth 1: ceo, vp  (both names are in mgr.reportsTo; _id "ceo" < "vp" -> ceo first)
//
// MongoDB scans the collection once per depth level (collection-scan order), so it
// emits ceo before vp at depth 1. The prior implementation iterated frontier values
// as the outer loop, emitting vp first (array element order), yielding [mgr, vp, ceo].
//
// After the fix the outer loop is fromDocs (sorted by _id), so ceo (id < vp) is
// discovered first: chain = [mgr, ceo, vp].
func TestAggStage_graphLookup_TraverseHierarchyFromLeaf(t *testing.T) {
	t.Parallel()

	// mgr.reportsTo is an array with "vp" listed BEFORE "ceo"  -- this is intentional.
	// It ensures that naive frontier-first iteration yields the wrong order [mgr,vp,ceo]
	// while the correct collection-scan-first iteration yields [mgr,ceo,vp].
	mgrReportsTo := types.MakeArray(2)
	mgrReportsTo.Append("vp")
	mgrReportsTo.Append("ceo")

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", "ceo", "name", "ceo")),
		must.NotFail(types.NewDocument("_id", "jr", "name", "jr", "reportsTo", "mgr")),
		must.NotFail(types.NewDocument("_id", "mgr", "name", "mgr", "reportsTo", mgrReportsTo)),
		must.NotFail(types.NewDocument("_id", "vp", "name", "vp")),
	}

	// Shuffle the fetcher order to prove the fix doesn't rely on fetcher ordering.
	shuffled := []*types.Document{employees[3], employees[0], employees[2], employees[1]} // vp, ceo, mgr, jr
	fetcher := makeFetcher(map[string][]*types.Document{"employees": shuffled})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "$reportsTo",
		"connectFromField", "reportsTo",
		"connectToField", "name",
		"as", "chain",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	// Input: the leaf employee "jr"
	inputDoc := must.NotFail(types.NewDocument("_id", "jr", "name", "jr", "reportsTo", "mgr"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	chainVal, err := results[0].Get("chain")
	if err != nil {
		t.Fatalf("missing chain field: %v", err)
	}

	chain, ok := chainVal.(*types.Array)
	if !ok {
		t.Fatalf("chain not an array: %T", chainVal)
	}

	// Expected: mgr (depth 0), ceo (depth 1, _id "ceo" < "vp"), vp (depth 1)
	if chain.Len() != 3 {
		t.Fatalf("expected 3 chain entries (mgr, ceo, vp), got %d", chain.Len())
	}

	// MongoDB emits within-level results in collection-scan (_id asc) order:
	//   depth 0: [mgr]
	//   depth 1: [ceo, vp]   <- "ceo" < "vp" alphabetically by _id
	wantNames := []string{"mgr", "ceo", "vp"}

	for i := 0; i < chain.Len(); i++ {
		v, _ := chain.Get(i)
		entry, ok := v.(*types.Document)
		if !ok {
			t.Errorf("chain[%d] not a document: %T", i, v)
			continue
		}

		nameVal, err := entry.Get("name")
		if err != nil {
			t.Errorf("chain[%d] missing name: %v", i, err)
			continue
		}

		if nameVal != wantNames[i] {
			t.Errorf("chain[%d].name = %v, want %v (MongoDB collection-scan order)", i, nameVal, wantNames[i])
		}
	}
}

// TestAggComplex_graphLookup_bfsOrder verifies that $graphLookup returns results in
// MongoDB's defined order: depth-0 results first, then remaining depth levels in
// reverse (deepest-first) order.
//
// Graph structure (org-chart with two branches):
//
//	root -> {A(_id=1), B(_id=2)}           depth 0
//	A    -> {C(_id=3)}                     depth 1
//	B    -> {D(_id=4)}                     depth 1
//	C    -> {E(_id=5)}                     depth 2
//
// Expected BFS visit order: A(d0), B(d0), C(d1), D(d1), E(d2)
// MongoDB output order: [d0...] then [dN, dN-1, ..., d1] ==> [A, B, E, C, D]
func TestAggComplex_graphLookup_bfsOrder(t *testing.T) {
	t.Parallel()

	employees := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "name", "A", "reportsTo", "root")),
		must.NotFail(types.NewDocument("_id", int32(2), "name", "B", "reportsTo", "root")),
		must.NotFail(types.NewDocument("_id", int32(3), "name", "C", "reportsTo", "A")),
		must.NotFail(types.NewDocument("_id", int32(4), "name", "D", "reportsTo", "B")),
		must.NotFail(types.NewDocument("_id", int32(5), "name", "E", "reportsTo", "C")),
	}

	fetcher := makeFetcher(map[string][]*types.Document{"employees": employees})

	spec := must.NotFail(types.NewDocument(
		"from", "employees",
		"startWith", "root",
		"connectFromField", "name",
		"connectToField", "reportsTo",
		"as", "tree",
	))
	stageDoc := must.NotFail(types.NewDocument("$graphLookup", spec))

	s, err := stages.NewGraphLookupStage(stageDoc, fetcher)
	if err != nil {
		t.Fatalf("NewGraphLookupStage: %v", err)
	}

	// Input: the root node (not in the employees collection)
	inputDoc := must.NotFail(types.NewDocument("_id", int32(0), "name", "root"))
	inputIter := iterator.Values(iterator.ForSlice([]*types.Document{inputDoc}))
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	outIter, err := s.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	results := collectResults(t, outIter, closer)

	if len(results) != 1 {
		t.Fatalf("expected 1 output doc, got %d", len(results))
	}

	treeVal, err := results[0].Get("tree")
	if err != nil {
		t.Fatalf("missing tree field: %v", err)
	}

	tree, ok := treeVal.(*types.Array)
	if !ok {
		t.Fatalf("tree not an array: %T", treeVal)
	}

	if tree.Len() != 5 {
		t.Fatalf("expected 5 tree entries (A, B, C, D, E), got %d", tree.Len())
	}

	// MongoDB output ordering: depth-0 first, then remaining levels deepest-first.
	//   L0 = [A, B]    -> first
	//   L1 = [C, D]    -> last (after deeper levels)
	//   L2 = [E]       -> second (deepest non-zero level)
	// Final: [A, B, E, C, D]
	wantNames := []string{"A", "B", "E", "C", "D"}

	for i := 0; i < tree.Len(); i++ {
		v, _ := tree.Get(i)
		entry, ok := v.(*types.Document)
		if !ok {
			t.Errorf("tree[%d] not a document: %T", i, v)
			continue
		}

		nameVal, err := entry.Get("name")
		if err != nil {
			t.Errorf("tree[%d] missing name: %v", i, err)
			continue
		}

		if nameVal != wantNames[i] {
			t.Errorf("tree[%d].name = %v, want %v (BFS level order)", i, nameVal, wantNames[i])
		}
	}
}

// TestAgg_sortByCount_after_unwind verifies that $unwind followed by $sortByCount
// correctly counts unwound array elements and sorts the results by count descending,
// with _id ascending as the tiebreaker per spec: $sortByCount === $group + $sort{count:-1, _id:1}.
func TestAgg_sortByCount_after_unwind(t *testing.T) {
	t.Parallel()

	// Three docs with tag arrays. Tag frequencies: x=2, y=2, z=1.
	// After $unwind: x,y from doc1; x from doc2; y,z from doc3 -> [x,y,x,y,z]
	doc1Tags := types.MakeArray(2)
	doc1Tags.Append("x")
	doc1Tags.Append("y")

	doc2Tags := types.MakeArray(1)
	doc2Tags.Append("x")

	doc3Tags := types.MakeArray(2)
	doc3Tags.Append("y")
	doc3Tags.Append("z")

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tags", doc1Tags)),
		must.NotFail(types.NewDocument("_id", int32(2), "tags", doc2Tags)),
		must.NotFail(types.NewDocument("_id", int32(3), "tags", doc3Tags)),
	}

	unwindDoc := must.NotFail(types.NewDocument("$unwind", "$tags"))
	unwindStage, err := stages.NewStage(unwindDoc)
	if err != nil {
		t.Fatalf("NewStage($unwind): %v", err)
	}

	sortByCountDoc := must.NotFail(types.NewDocument("$sortByCount", "$tags"))
	sortByCountStage, err := stages.NewStage(sortByCountDoc)
	if err != nil {
		t.Fatalf("NewStage($sortByCount): %v", err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	inputIter := iterator.Values(iterator.ForSlice(docs))

	out, err := unwindStage.Process(context.Background(), inputIter, closer)
	if err != nil {
		t.Fatalf("$unwind Process: %v", err)
	}

	out, err = sortByCountStage.Process(context.Background(), out, closer)
	if err != nil {
		t.Fatalf("$sortByCount Process: %v", err)
	}

	results := collectResults(t, out, closer)

	// "x" count=2, "y" count=2, "z" count=1.
	// Tie at count=2: sorted by _id ascending -> "x" < "y".
	// Expected: [{_id:"x",count:2}, {_id:"y",count:2}, {_id:"z",count:1}]
	if len(results) != 3 {
		t.Fatalf("expected 3 result docs, got %d", len(results))
	}

	type entry struct{ id string; count int32 }
	want := []entry{{"x", 2}, {"y", 2}, {"z", 1}}

	for i, w := range want {
		idVal, err := results[i].Get("_id")
		if err != nil {
			t.Errorf("results[%d] missing _id: %v", i, err)
			continue
		}

		countVal, err := results[i].Get("count")
		if err != nil {
			t.Errorf("results[%d] missing count: %v", i, err)
			continue
		}

		if idVal != w.id {
			t.Errorf("results[%d]._id = %v, want %q", i, idVal, w.id)
		}

		count, ok := countVal.(int32)
		if !ok {
			t.Errorf("results[%d].count is %T, want int32", i, countVal)
			continue
		}

		if count != w.count {
			t.Errorf("results[%d].count = %d, want %d", i, count, w.count)
		}
	}
}

// TestAggComplex_matchUnwindGroupSort_SameTotalQty verifies that $sort is stable when
// ALL groups have the same sort key value. In this case the sort must preserve the
// insertion order produced by the preceding $group stage, which itself preserves the
// order in which groups were first seen.
//
// Three docs: C, B, A  -- each with qty=5. After $group by category (sum of qty),
// all groups have totalQty=5. After $sort by totalQty desc (fully tied), stable sort
// must preserve group-creation order: C, B, A.
func TestAggComplex_matchUnwindGroupSort_SameTotalQty(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "category", "C", "qty", int32(5))),
		must.NotFail(types.NewDocument("_id", int32(2), "category", "B", "qty", int32(5))),
		must.NotFail(types.NewDocument("_id", int32(3), "category", "A", "qty", int32(5))),
	}

	// $group: {_id: "$category", totalQty: {$sum: "$qty"}}
	groupSpec := must.NotFail(types.NewDocument(
		"_id", "$category",
		"totalQty", must.NotFail(types.NewDocument("$sum", "$qty")),
	))
	groupDoc := must.NotFail(types.NewDocument("$group", groupSpec))
	groupStage, err := stages.NewStage(groupDoc)
	if err != nil {
		t.Fatalf("NewStage($group): %v", err)
	}

	// $sort: {totalQty: -1}
	sortDoc := must.NotFail(types.NewDocument("$sort", must.NotFail(types.NewDocument("totalQty", int32(-1)))))
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

	if len(results) != 3 {
		t.Fatalf("expected 3 output docs, got %d", len(results))
	}

	getID := func(doc *types.Document) string {
		v, _ := doc.Get("_id")
		s, _ := v.(string)
		return s
	}

	// All three have totalQty=5. Stable sort must preserve first-seen order: C, B, A.
	wantIDs := []string{"C", "B", "A"}
	for i, want := range wantIDs {
		if id := getID(results[i]); id != want {
			t.Errorf("results[%d]: expected _id=%s (stable tie-break), got %q", i, want, id)
		}
	}
}

// TestAggComplex_sortByCount verifies the full $sortByCount behavior:
// primary sort by count descending, tiebreaker by _id ascending per spec.
//
// Input: 5 docs with tags a(x3), b(x2), c(x2), d(x1).
// Expected: [{_id:"a",count:3}, {_id:"b",count:2}, {_id:"c",count:2}, {_id:"d",count:1}]
// b and c have the same count=2; ascending _id -> "b" before "c".
func TestAggComplex_sortByCount(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "a")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "a")),
		must.NotFail(types.NewDocument("_id", int32(3), "tag", "a")),
		must.NotFail(types.NewDocument("_id", int32(4), "tag", "b")),
		must.NotFail(types.NewDocument("_id", int32(5), "tag", "b")),
		must.NotFail(types.NewDocument("_id", int32(6), "tag", "c")),
		must.NotFail(types.NewDocument("_id", int32(7), "tag", "c")),
		must.NotFail(types.NewDocument("_id", int32(8), "tag", "d")),
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

	if len(results) != 4 {
		t.Fatalf("expected 4 result docs, got %d", len(results))
	}

	type entry struct {
		id    string
		count int32
	}

	want := []entry{{"a", 3}, {"b", 2}, {"c", 2}, {"d", 1}}

	for i, w := range want {
		idVal, err := results[i].Get("_id")
		if err != nil {
			t.Errorf("results[%d] missing _id: %v", i, err)
			continue
		}

		countVal, err := results[i].Get("count")
		if err != nil {
			t.Errorf("results[%d] missing count: %v", i, err)
			continue
		}

		if idVal != w.id {
			t.Errorf("results[%d]._id = %v, want %q", i, idVal, w.id)
		}

		count, ok := countVal.(int32)
		if !ok {
			t.Errorf("results[%d].count is %T, want int32", i, countVal)
			continue
		}

		if count != w.count {
			t.Errorf("results[%d].count = %d, want %d", i, count, w.count)
		}
	}
}

// TestAggComplex_sortByCount_TieBreaking verifies that $sortByCount uses _id ascending
// as a tiebreaker when multiple groups share the same count, per spec:
// $sortByCount === $group + $sort{count:-1, _id:1}.
//
// Input: 3 docs each with a unique tag: "z", "m", "a".
// Expected: [{_id:"a",count:1}, {_id:"m",count:1}, {_id:"z",count:1}]  -- ascending _id.
func TestAggComplex_sortByCount_TieBreaking(t *testing.T) {
	t.Parallel()

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "z")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "m")),
		must.NotFail(types.NewDocument("_id", int32(3), "tag", "a")),
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

	// All counts are 1  -- tiebreaker: _id ascending -> a, m, z.
	wantIDs := []string{"a", "m", "z"}

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

		if count != 1 {
			t.Errorf("results[%d].count = %d, want 1", i, count)
		}
	}
}
