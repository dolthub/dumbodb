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

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestMongoDB_GroupAndTotalTutorial verifies the $match -> $sort -> $group ($first, $sum, $push)
// -> $sort -> $set -> $unset pipeline from the MongoDB group-and-total tutorial. (DumboDBFull)
//
// https://www.mongodb.com/docs/manual/tutorial/aggregation-examples/group-and-total/
//
// Exercises:
//   - $push with a document expression evaluates field references per document
//   - $set with a string field reference (e.g. "$_id") evaluates the expression
//   - $unset ["_id"] removes the _id field produced by $group
//   - $first "$ord_date" preserves datetime values
func TestMongoDB_GroupAndTotalTutorial(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.collection(t)
	ctx := context.Background()

	// Dates used in test data.
	mar01 := time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	mar08 := time.Date(2020, 3, 8, 0, 0, 0, 0, time.UTC)
	jan15 := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	apr18 := time.Date(2020, 4, 18, 0, 0, 0, 0, time.UTC)

	// Three customers; Ant O. Knee has two orders, Busby Bee one, Cam Elot one.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("cust_id", "Ant O. Knee"), e("ord_date", mar01), e("price", int32(25)), e("status", "A")),
		d(e("_id", int32(2)), e("cust_id", "Ant O. Knee"), e("ord_date", mar08), e("price", int32(70)), e("status", "A")),
		d(e("_id", int32(3)), e("cust_id", "Busby Bee"), e("ord_date", jan15), e("price", int32(35)), e("status", "A")),
		d(e("_id", int32(4)), e("cust_id", "Cam Elot"), e("ord_date", apr18), e("price", int32(25)), e("status", "A")),
	)

	yr2020Start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	yr2021Start := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

	pipeline := bson.A{
		// Stage 1: filter to year 2020.
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "ord_date", Value: bson.D{
				{Key: "$gte", Value: yr2020Start},
				{Key: "$lt", Value: yr2021Start},
			}},
		}}},
		// Stage 2: sort so $first / $push preserve order per customer.
		bson.D{{Key: "$sort", Value: bson.D{
			{Key: "cust_id", Value: int32(1)},
			{Key: "ord_date", Value: int32(1)},
		}}},
		// Stage 3: group by customer, accumulate.
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$cust_id"},
			{Key: "firstPurchaseDate", Value: bson.D{{Key: "$first", Value: "$ord_date"}}},
			{Key: "totalValue", Value: bson.D{{Key: "$sum", Value: "$price"}}},
			{Key: "totalNum", Value: bson.D{{Key: "$sum", Value: int32(1)}}},
			{Key: "orders", Value: bson.D{{Key: "$push", Value: bson.D{
				{Key: "amount", Value: "$price"},
				{Key: "date", Value: "$ord_date"},
			}}}},
		}}},
		// Stage 4: sort by descending total.
		bson.D{{Key: "$sort", Value: bson.D{{Key: "totalValue", Value: int32(-1)}}}},
		// Stage 5: rename _id to customer via $set with a field reference.
		bson.D{{Key: "$set", Value: bson.D{{Key: "customer", Value: "$_id"}}}},
		// Stage 6: drop the original _id.
		bson.D{{Key: "$unset", Value: bson.A{"_id"}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3, "one result per customer")

	// Results are sorted by totalValue descending:
	//   Ant O. Knee: 25 + 70 = 95
	//   Busby Bee: 35
	//   Cam Elot: 25

	r0 := dmap(results[0])
	r1 := dmap(results[1])
	r2 := dmap(results[2])

	// --- Result 0: Ant O. Knee (highest total) ---

	// $set: { customer: "$_id" } must evaluate "$_id" as a field reference,
	// not store the literal string "$_id".
	assert.Equal(t, "Ant O. Knee", r0["customer"], "customer must be the group _id value, not the literal string \"$_id\"")

	// $unset: ["_id"] must remove the _id field produced by $group.
	_, hasID := r0["_id"]
	assert.False(t, hasID, "_id must be removed by $unset")

	assert.EqualValues(t, 95, r0["totalValue"])
	assert.EqualValues(t, 2, r0["totalNum"])

	// $first "$ord_date" must return the datetime from the first sorted document.
	// The driver decodes BSON Date as bson.DateTime (int64 ms since epoch).
	fpd0 := r0["firstPurchaseDate"]
	assertDateTimeEqual(t, mar01, fpd0, "firstPurchaseDate for Ant O. Knee must be 2020-03-01")

	// $push: { amount: "$price", date: "$ord_date" } must evaluate field refs per input doc.
	orders0, ok := r0["orders"].(bson.A)
	require.True(t, ok, "orders must be a BSON array, got %T", r0["orders"])
	require.Len(t, orders0, 2, "Ant O. Knee has 2 orders")

	o00 := orders0[0].(bson.M)
	assert.EqualValues(t, 25, o00["amount"], "first order amount must be 25, not the literal string \"$price\"")
	assertDateTimeEqual(t, mar01, o00["date"], "first order date must be 2020-03-01")

	o01 := orders0[1].(bson.M)
	assert.EqualValues(t, 70, o01["amount"], "second order amount must be 70")
	assertDateTimeEqual(t, mar08, o01["date"], "second order date must be 2020-03-08")

	// --- Result 1: Busby Bee ---
	assert.Equal(t, "Busby Bee", r1["customer"])
	_, hasID1 := r1["_id"]
	assert.False(t, hasID1)
	assert.EqualValues(t, 35, r1["totalValue"])
	assert.EqualValues(t, 1, r1["totalNum"])

	orders1, ok := r1["orders"].(bson.A)
	require.True(t, ok)
	require.Len(t, orders1, 1)
	o10 := orders1[0].(bson.M)
	assert.EqualValues(t, 35, o10["amount"])
	assertDateTimeEqual(t, jan15, o10["date"], "Busby Bee order date must be 2020-01-15")

	// --- Result 2: Cam Elot ---
	assert.Equal(t, "Cam Elot", r2["customer"])
	_, hasID2 := r2["_id"]
	assert.False(t, hasID2)
	assert.EqualValues(t, 25, r2["totalValue"])
}

// assertDateTimeEqual checks that a BSON date value (bson.DateTime or time.Time)
// equals the expected time.
func assertDateTimeEqual(t *testing.T, want time.Time, got any, msgAndArgs ...any) {
	t.Helper()

	switch v := got.(type) {
	case bson.DateTime:
		assert.True(t, want.Equal(v.Time()), msgAndArgs...)
	case time.Time:
		assert.True(t, want.Equal(v), msgAndArgs...)
	default:
		assert.Fail(t, "expected a date/time value", "got %T (%v)", got, got)
	}
}
