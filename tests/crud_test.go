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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestUpdateMany_upsert verifies that UpdateMany with upsert:true inserts a new
// document when the filter matches no existing documents. (DongoFull)
func TestUpdateMany_upsert(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	// No documents in the collection; upsert should insert one.
	res, err := coll.UpdateMany(ctx,
		d(e("status", "pending")),
		d(e("$set", d(e("status", "pending"), e("count", int32(1))))),
		options.Update().SetUpsert(true),
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.MatchedCount, "no documents should match before upsert")
	assert.NotNil(t, res.UpsertedID, "upsert must produce an UpsertedID")

	// Verify the document was inserted.
	count, err := coll.CountDocuments(ctx, d(e("status", "pending")))
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "upserted document must be visible")
}

// TestFindOneAndUpdate_returnAfter verifies that FindOneAndUpdate with
// ReturnDocument(After) returns the document as it looks after the update. (DongoFull)
func TestFindOneAndUpdate_returnAfter(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
	)

	ctx := context.Background()

	var result bson.D
	err := coll.FindOneAndUpdate(ctx,
		d(e("_id", int32(1))),
		d(e("$set", d(e("x", int32(2))))),
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&result)
	require.NoError(t, err)

	// Find the "x" field in the returned document.
	var xVal int32
	for _, el := range result {
		if el.Key == "x" {
			xVal = el.Value.(int32)
		}
	}
	assert.Equal(t, int32(2), xVal, "ReturnDocument(After) must return the updated value")
}

// TestFindOneAndReplace_no_match verifies that FindOneAndReplace returns
// ErrNoDocuments when the filter does not match any document. (DongoFull)
func TestFindOneAndReplace_no_match(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Collection is empty; filter cannot match.
	var result bson.D
	err := coll.FindOneAndReplace(ctx,
		d(e("_id", int32(999))),
		d(e("x", int32(1))),
	).Decode(&result)
	assert.ErrorIs(t, err, mongo.ErrNoDocuments, "FindOneAndReplace with no match must return ErrNoDocuments")
}

// TestCountDocuments_nested_filter verifies that CountDocuments correctly filters
// documents using dot-notation for nested fields. (DongoFull)
func TestCountDocuments_nested_filter(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("a", d(e("b", int32(5))))),
		d(e("_id", int32(2)), e("a", d(e("b", int32(10))))),
		d(e("_id", int32(3)), e("x", int32(1))),
	)

	ctx := context.Background()

	count, err := coll.CountDocuments(ctx, d(e("a.b", int32(5))))
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "nested field filter must match exactly one document")
}

// TestEstimatedDocumentCount_empty verifies that EstimatedDocumentCount returns
// 0 for an empty collection. (DongoFull)
func TestEstimatedDocumentCount_empty(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Collection has no documents; estimated count must be 0.
	count, err := coll.EstimatedDocumentCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "EstimatedDocumentCount on empty collection must return 0")
}
