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
	"testing"

	"github.com/dolthub/dongo/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/must"
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

	// Local doc has colorRefs: ["red", "green"] — should match items 1 and 3.
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
	// No let needed — this is an uncorrelated sub-pipeline that fetches all orders
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

	// colorRefs: ["blue", "green"] — no intersection with items collection.
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
// Graph: alice → bob → carol  (each employee's reportsTo field points to their manager's name).
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
// Graph: a ↔ b (a.reportsTo=b, b.reportsTo=a). The traversal should terminate
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
	// "a" was the search value that started it all — actually "b" was searched first,
	// then "a" is queued. "a" has not been searched yet, so we look for connectToField=a
	// and find employee "a". But then a.reportsTo=b which was already searched, so we stop.
	// Result: b and a → 2 documents.
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
// $match → $group (with $addToSet) → $project produces deterministically
// ordered set elements matching MongoDB's sorted output.
func TestAggComplex_matchGroupProject_addToSet(t *testing.T) {
	t.Parallel()

	// Three orders with two distinct statuses: "pending" appears first in the
	// slice, "cancelled" second. Without explicit sorting in $addToSet the
	// output order is document-iteration-dependent; with sorting it is stable.
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

	// MongoDB returns $addToSet elements sorted; we expect ["cancelled", "pending"].
	s0, _ := statuses.Get(0)
	s1, _ := statuses.Get(1)

	if s0 != "cancelled" || s1 != "pending" {
		t.Errorf("expected [cancelled pending], got [%v %v]", s0, s1)
	}
}

// TestAggStage_graphLookup_MaxDepthLimitsTraversal verifies that maxDepth correctly
// limits traversal and that depthField values are returned as int32 (matching MongoDB).
//
// Graph: alice → bob → carol → dave → eve (5-node chain)
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

	// Verify depthField is int32 (matching MongoDB BSON wire type) and values are 0, 1, 2.
	expectedNames := []string{"bob", "carol", "dave"}
	expectedDepths := []int32{0, 1, 2}

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
