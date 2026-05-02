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
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestCRUD_FindOneAndUpdate_Basic verifies findOneAndUpdate returns the OLD document
// by default and applies the update. (DumboDBFull)
func TestCRUD_FindOneAndUpdate_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "alice"}, {Key: "score", Value: int32(10)}},
	)

	var before bson.D
	err := coll.FindOneAndUpdate(ctx,
		bson.D{{Key: "name", Value: "alice"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(20)}}}},
	).Decode(&before)
	require.NoError(t, err, "findOneAndUpdate must not error on existing document")

	// Default ReturnDocument=Before  -- the result is the original doc.
	m := dmap(before)
	assert.Equal(t, "alice", m["name"])
	assert.Equal(t, int32(10), m["score"], "default behavior returns document BEFORE update")

	// Verify the update was actually applied.
	var after bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "name", Value: "alice"}}).Decode(&after))
	assert.Equal(t, int32(20), dmap(after)["score"], "document must reflect update")
}

// TestCRUD_FindOneAndUpdate_ReturnAfter verifies findOneAndUpdate with
// ReturnDocument=After returns the UPDATED document. (DumboDBFull)
func TestCRUD_FindOneAndUpdate_ReturnAfter(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "bob"}, {Key: "score", Value: int32(5)}},
	)

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var result bson.D
	err := coll.FindOneAndUpdate(ctx,
		bson.D{{Key: "name", Value: "bob"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(99)}}}},
		opts,
	).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, int32(99), dmap(result)["score"], "ReturnDocument=After must return updated document")
}

// TestCRUD_FindOneAndUpdate_Upsert verifies that findOneAndUpdate with Upsert=true
// inserts a document when no match is found. (DumboDBFull)
func TestCRUD_FindOneAndUpdate_Upsert(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)

	var result bson.D
	err := coll.FindOneAndUpdate(ctx,
		bson.D{{Key: "name", Value: "charlie"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(42)}}}},
		opts,
	).Decode(&result)
	require.NoError(t, err, "findOneAndUpdate with upsert must insert a new document")

	m := dmap(result)
	assert.Equal(t, "charlie", m["name"])
	assert.Equal(t, int32(42), m["score"])

	// Verify exactly one document was created.
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "name", Value: "charlie"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// TestCRUD_FindOneAndUpdate_NoMatch verifies that findOneAndUpdate returns
// ErrNoDocuments when the filter matches nothing and Upsert is false. (DumboDBFull)
func TestCRUD_FindOneAndUpdate_NoMatch(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	err := coll.FindOneAndUpdate(ctx,
		bson.D{{Key: "name", Value: "ghost"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(1)}}}},
	).Err()
	assert.ErrorIs(t, err, mongo.ErrNoDocuments, "no-match without upsert must return ErrNoDocuments")
}

// TestCRUD_FindOneAndUpdate_Projection verifies that findOneAndUpdate with a
// projection returns only specified fields. (DumboDBFull)
func TestCRUD_FindOneAndUpdate_Projection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{
			{Key: "name", Value: "diana"},
			{Key: "score", Value: int32(7)},
			{Key: "extra", Value: "hidden"},
		},
	)

	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetProjection(bson.D{{Key: "score", Value: 1}, {Key: "_id", Value: 0}})

	var result bson.D
	require.NoError(t, coll.FindOneAndUpdate(ctx,
		bson.D{{Key: "name", Value: "diana"}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "score", Value: int32(3)}}}},
		opts,
	).Decode(&result))

	m := dmap(result)
	assert.Equal(t, int32(10), m["score"])
	assert.Nil(t, m["name"], "projected-out field must not appear")
	assert.Nil(t, m["extra"], "projected-out field must not appear")
}

// TestCRUD_FindOneAndDelete_Basic verifies findOneAndDelete removes a matching
// document and returns it. (DumboDBFull)
func TestCRUD_FindOneAndDelete_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "eve"}, {Key: "score", Value: int32(3)}},
		bson.D{{Key: "name", Value: "frank"}, {Key: "score", Value: int32(8)}},
	)

	var deleted bson.D
	err := coll.FindOneAndDelete(ctx,
		bson.D{{Key: "name", Value: "eve"}},
	).Decode(&deleted)
	require.NoError(t, err, "findOneAndDelete must not error on existing document")
	assert.Equal(t, "eve", dmap(deleted)["name"])

	// Verify the document is gone.
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "name", Value: "eve"}})
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "deleted document must no longer exist")

	// Other documents must remain.
	count, err = coll.CountDocuments(ctx, bson.D{{Key: "name", Value: "frank"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// TestCRUD_FindOneAndDelete_NoMatch verifies that findOneAndDelete returns
// ErrNoDocuments when the filter matches nothing. (DumboDBFull)
func TestCRUD_FindOneAndDelete_NoMatch(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	err := coll.FindOneAndDelete(ctx,
		bson.D{{Key: "name", Value: "nobody"}},
	).Err()
	assert.ErrorIs(t, err, mongo.ErrNoDocuments)
}

// TestCRUD_FindOneAndDelete_Sort verifies that findOneAndDelete with a sort option
// deletes the document selected by the sort. (DumboDBFull)
func TestCRUD_FindOneAndDelete_Sort(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "score", Value: int32(5)}},
		bson.D{{Key: "score", Value: int32(1)}},
		bson.D{{Key: "score", Value: int32(9)}},
	)

	// Delete the document with the lowest score.
	opts := options.FindOneAndDelete().SetSort(bson.D{{Key: "score", Value: 1}})
	var result bson.D
	require.NoError(t, coll.FindOneAndDelete(ctx, bson.D{}, opts).Decode(&result))
	assert.Equal(t, int32(1), dmap(result)["score"], "sort ascending should delete the lowest-score doc")

	// Two documents must remain.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)
}

// TestCRUD_UpdateMany_Basic verifies that UpdateMany applies the update to all
// matching documents. (DumboDBFull)
func TestCRUD_UpdateMany_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "status", Value: "active"}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "status", Value: "active"}, {Key: "v", Value: int32(2)}},
		bson.D{{Key: "status", Value: "inactive"}, {Key: "v", Value: int32(3)}},
	)

	result, err := coll.UpdateMany(ctx,
		bson.D{{Key: "status", Value: "active"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "updated", Value: true}}}},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.MatchedCount, "must match 2 active documents")
	assert.Equal(t, int64(2), result.ModifiedCount, "must modify 2 active documents")

	// Verify both active docs were updated.
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "updated", Value: true}})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	// Verify inactive doc was untouched.
	var inactive bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "status", Value: "inactive"}}).Decode(&inactive))
	assert.Nil(t, dmap(inactive)["updated"], "inactive document must not have 'updated' field")
}

// TestCRUD_UpdateMany_Upsert verifies UpdateMany with Upsert inserts when no
// document matches the filter. (DumboDBFull)
func TestCRUD_UpdateMany_Upsert(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	opts := options.UpdateMany().SetUpsert(true)
	result, err := coll.UpdateMany(ctx,
		bson.D{{Key: "name", Value: "upserted"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "created", Value: true}}}},
		opts,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.MatchedCount)
	assert.Equal(t, int64(1), result.UpsertedCount, "must upsert one document")

	count, err := coll.CountDocuments(ctx, bson.D{{Key: "name", Value: "upserted"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// TestCRUD_DeleteMany_Basic verifies DeleteMany removes all matching documents. (DumboDBFull)
func TestCRUD_DeleteMany_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "tag", Value: "delete-me"}, {Key: "n", Value: int32(1)}},
		bson.D{{Key: "tag", Value: "delete-me"}, {Key: "n", Value: int32(2)}},
		bson.D{{Key: "tag", Value: "keep-me"}, {Key: "n", Value: int32(3)}},
	)

	result, err := coll.DeleteMany(ctx, bson.D{{Key: "tag", Value: "delete-me"}})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.DeletedCount, "must delete 2 matching documents")

	// Verify the keeper remains.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// TestCRUD_DeleteMany_EmptyFilter verifies DeleteMany with an empty filter
// removes all documents in the collection. (DumboDBFull)
func TestCRUD_DeleteMany_EmptyFilter(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "x", Value: int32(1)}},
		bson.D{{Key: "x", Value: int32(2)}},
		bson.D{{Key: "x", Value: int32(3)}},
	)

	result, err := coll.DeleteMany(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), result.DeletedCount)

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// TestCRUD_DeleteMany_NoMatch verifies DeleteMany returns 0 deleted when no
// documents match. (DumboDBFull)
func TestCRUD_DeleteMany_NoMatch(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	result, err := coll.DeleteMany(ctx, bson.D{{Key: "x", Value: int32(999)}})
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.DeletedCount)
}

// TestCRUD_BulkWrite_InsertUpdateDelete verifies that BulkWrite correctly
// mixes InsertOne, UpdateOne, and DeleteOne operations. (DumboDBFull)
func TestCRUD_BulkWrite_InsertUpdateDelete(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Seed two existing documents.
	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "to-update"}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "name", Value: "to-delete"}, {Key: "v", Value: int32(2)}},
	)

	models := []mongo.WriteModel{
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "name", Value: "inserted"}, {Key: "v", Value: int32(3)}}),
		mongo.NewUpdateOneModel().
			SetFilter(bson.D{{Key: "name", Value: "to-update"}}).
			SetUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(10)}}}}),
		mongo.NewDeleteOneModel().SetFilter(bson.D{{Key: "name", Value: "to-delete"}}),
	}

	result, err := coll.BulkWrite(ctx, models)
	require.NoError(t, err, "BulkWrite must succeed")
	assert.Equal(t, int64(1), result.InsertedCount)
	assert.Equal(t, int64(1), result.ModifiedCount)
	assert.Equal(t, int64(1), result.DeletedCount)

	// Verify final state.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count, "must have 2 documents: inserted + updated, deleted one removed")

	var updated bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "name", Value: "to-update"}}).Decode(&updated))
	assert.Equal(t, int32(10), dmap(updated)["v"])

	err = coll.FindOne(ctx, bson.D{{Key: "name", Value: "to-delete"}}).Err()
	assert.ErrorIs(t, err, mongo.ErrNoDocuments, "deleted document must be gone")
}

// TestCRUD_BulkWrite_Ordered verifies that ordered BulkWrite stops on first error. (DumboDBFull)
func TestCRUD_BulkWrite_Ordered(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Create a unique index on "key".
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "key", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)

	insertDocs(t, coll, bson.D{{Key: "key", Value: "exists"}})

	models := []mongo.WriteModel{
		// This will succeed.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "new"}}),
		// This will fail  -- duplicate key.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "exists"}}),
		// This would succeed but must not run in ordered mode.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "after-error"}}),
	}

	opts := options.BulkWrite().SetOrdered(true)
	_, err = coll.BulkWrite(ctx, models, opts)
	require.Error(t, err, "ordered BulkWrite must return an error on duplicate key")

	// The first insert succeeded; the third must NOT have run.
	count, cErr := coll.CountDocuments(ctx, bson.D{{Key: "key", Value: "after-error"}})
	require.NoError(t, cErr)
	assert.Equal(t, int64(0), count, "ordered BulkWrite stops after error  -- third op must not execute")
}

// TestCRUD_BulkWrite_Unordered verifies that unordered BulkWrite continues after
// errors and reports them in the result. (DumboDBFull)
func TestCRUD_BulkWrite_Unordered(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "key", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)

	insertDocs(t, coll, bson.D{{Key: "key", Value: "exists"}})

	models := []mongo.WriteModel{
		// Succeed.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "good1"}}),
		// Fail  -- duplicate.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "exists"}}),
		// Should still run in unordered mode.
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "key", Value: "good2"}}),
	}

	opts := options.BulkWrite().SetOrdered(false)
	result, err := coll.BulkWrite(ctx, models, opts)
	// An error is expected, but non-failing inserts must have proceeded.
	require.Error(t, err)
	assert.Equal(t, int64(2), result.InsertedCount, "unordered BulkWrite must process non-error ops")
}

// TestCRUD_BulkWrite_ReplaceOne verifies that ReplaceOne in a BulkWrite
// fully replaces a document (not a partial update). (DumboDBFull)
func TestCRUD_BulkWrite_ReplaceOne(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "original"}, {Key: "extra", Value: "value"}},
	)

	models := []mongo.WriteModel{
		mongo.NewReplaceOneModel().
			SetFilter(bson.D{{Key: "name", Value: "original"}}).
			SetReplacement(bson.D{{Key: "name", Value: "replaced"}}),
	}

	result, err := coll.BulkWrite(ctx, models)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.ModifiedCount)

	var doc bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "name", Value: "replaced"}}).Decode(&doc))
	m := dmap(doc)
	assert.Equal(t, "replaced", m["name"])
	assert.Nil(t, m["extra"], "ReplaceOne must drop fields not in the replacement document")
}

// TestCRUD_UpdateOne_SetOnInsert verifies $setOnInsert only sets fields during
// an upsert insert (not when a document is found). (DumboDBFull)
func TestCRUD_UpdateOne_SetOnInsert(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	opts := options.UpdateOne().SetUpsert(true)

	// First call: no match → upsert insert. $setOnInsert must apply.
	result, err := coll.UpdateOne(ctx,
		bson.D{{Key: "key", Value: "soi-test"}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "key", Value: "soi-test"}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created", Value: true}}},
		},
		opts,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.UpsertedCount)

	var doc bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "key", Value: "soi-test"}}).Decode(&doc))
	assert.Equal(t, true, dmap(doc)["created"], "$setOnInsert must set 'created' on upsert insert")

	// Second call: now a document exists → update (not insert). $setOnInsert must NOT change 'created'.
	result, err = coll.UpdateOne(ctx,
		bson.D{{Key: "key", Value: "soi-test"}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "touched", Value: true}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created", Value: false}}},
		},
		opts,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.UpsertedCount, "second call must update, not upsert")

	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "key", Value: "soi-test"}}).Decode(&doc))
	assert.Equal(t, true, dmap(doc)["created"], "$setOnInsert must NOT change 'created' on update")
}

// TestCRUD_InsertMany_DuplicateKey verifies InsertMany returns a BulkWriteException
// when a duplicate _id is inserted (ordered=true). (DumboDBFull)
func TestCRUD_InsertMany_DuplicateKey(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	docs := []interface{}{
		bson.D{{Key: "_id", Value: "dup"}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "_id", Value: "dup"}, {Key: "v", Value: int32(2)}}, // duplicate
	}

	_, err := coll.InsertMany(ctx, docs)
	require.Error(t, err, "InsertMany with duplicate _id must error")

	var bwe mongo.BulkWriteException
	require.ErrorAs(t, err, &bwe)
	assert.NotEmpty(t, bwe.WriteErrors, "must have write errors for the duplicate")
}

// TestCRUD_InsertMany_Unordered verifies InsertMany with ordered=false
// continues inserting non-conflicting documents after a duplicate key error. (DumboDBFull)
func TestCRUD_InsertMany_Unordered(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	docs := []interface{}{
		bson.D{{Key: "_id", Value: "a"}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "_id", Value: "a"}, {Key: "v", Value: int32(2)}}, // duplicate
		bson.D{{Key: "_id", Value: "b"}, {Key: "v", Value: int32(3)}},
	}

	opts := options.InsertMany().SetOrdered(false)
	_, err := coll.InsertMany(ctx, docs, opts)
	require.Error(t, err, "unordered InsertMany must still return error for the duplicate")

	// Both "a" (first) and "b" (third) must have been inserted.
	count, cErr := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, cErr)
	assert.Equal(t, int64(2), count, "unordered InsertMany must insert non-conflicting docs")
}

// TestCRUD_ReplaceOne_Basic verifies ReplaceOne fully replaces a document. (DumboDBFull)
func TestCRUD_ReplaceOne_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "old"}, {Key: "extra", Value: "data"}},
	)

	result, err := coll.ReplaceOne(ctx,
		bson.D{{Key: "name", Value: "old"}},
		bson.D{{Key: "name", Value: "new"}},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.MatchedCount)
	assert.Equal(t, int64(1), result.ModifiedCount)

	var doc bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "name", Value: "new"}}).Decode(&doc))
	assert.Nil(t, dmap(doc)["extra"], "ReplaceOne must remove fields not in replacement")
}

// TestCRUD_CountDocuments_WithFilter verifies CountDocuments returns the correct
// count for a filtered query. (DumboDBFull)
func TestCRUD_CountDocuments_WithFilter(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "color", Value: "red"}},
		bson.D{{Key: "color", Value: "red"}},
		bson.D{{Key: "color", Value: "blue"}},
	)

	redCount, err := coll.CountDocuments(ctx, bson.D{{Key: "color", Value: "red"}})
	require.NoError(t, err)
	assert.Equal(t, int64(2), redCount)

	totalCount, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), totalCount)
}

// TestCRUD_Distinct_Basic verifies Distinct returns unique values for a field. (DumboDBFull)
func TestCRUD_Distinct_Basic(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "cat", Value: "A"}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "cat", Value: "A"}, {Key: "v", Value: int32(2)}},
		bson.D{{Key: "cat", Value: "B"}, {Key: "v", Value: int32(3)}},
	)

	var values []any
	require.NoError(t, coll.Distinct(ctx, "cat", bson.D{}).Decode(&values))
	assert.Len(t, values, 2, "Distinct must return 2 unique categories")
	// Values may be in any order.
	found := map[string]bool{}
	for _, v := range values {
		found[v.(string)] = true
	}
	assert.True(t, found["A"] && found["B"])
}

// TestCRUD_Distinct_WithFilter verifies Distinct respects a filter. (DumboDBFull)
func TestCRUD_Distinct_WithFilter(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "group", Value: "x"}, {Key: "val", Value: int32(1)}},
		bson.D{{Key: "group", Value: "x"}, {Key: "val", Value: int32(2)}},
		bson.D{{Key: "group", Value: "y"}, {Key: "val", Value: int32(3)}},
	)

	var vals []any
	require.NoError(t, coll.Distinct(ctx, "val", bson.D{{Key: "group", Value: "x"}}).Decode(&vals))
	assert.Len(t, vals, 2)
}

// TestCRUD_Distinct_Indexed exercises the index-driven fast path: the field is
// indexed and the distinct request has no filter, so the backend should serve
// the query from a sorted secondary index walk rather than a full scan.
// (DumboDBFull)
func TestCRUD_Distinct_Indexed(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "cat", Value: 1}},
	})
	require.NoError(t, err)

	// Insert duplicates of three values plus one document missing the field.
	insertDocs(t, coll,
		bson.D{{Key: "cat", Value: "A"}},
		bson.D{{Key: "cat", Value: "A"}},
		bson.D{{Key: "cat", Value: "B"}},
		bson.D{{Key: "cat", Value: "C"}},
		bson.D{{Key: "cat", Value: "C"}},
		bson.D{{Key: "cat", Value: "C"}},
		bson.D{{Key: "other", Value: "no-cat"}},
	)

	var values []any
	require.NoError(t, coll.Distinct(ctx, "cat", bson.D{}).Decode(&values))
	require.Len(t, values, 3)
	got := map[string]bool{}
	for _, v := range values {
		got[v.(string)] = true
	}
	assert.True(t, got["A"] && got["B"] && got["C"])
}

// TestCRUD_Distinct_NumericTypes verifies that numerically-equal values
// across int32/int64/double dedup to a single distinct entry, matching
// MongoDB semantics. (DumboDBFull)
func TestCRUD_Distinct_NumericTypes(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "n", Value: int32(5)}},
		bson.D{{Key: "n", Value: int64(5)}},
		bson.D{{Key: "n", Value: float64(5.0)}},
		bson.D{{Key: "n", Value: float64(5.5)}},
		bson.D{{Key: "n", Value: int32(7)}},
	)

	var values []any
	require.NoError(t, coll.Distinct(ctx, "n", bson.D{}).Decode(&values))
	// 5, 5.5, 7  -- three distinct numeric values regardless of source type.
	assert.Len(t, values, 3)
}
func TestUpdateMany_upsert(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	ctx := context.Background()

	// No documents in the collection; upsert should insert one.
	res, err := coll.UpdateMany(ctx,
		d(e("status", "pending")),
		d(e("$set", d(e("status", "pending"), e("count", int32(1))))),
		options.UpdateMany().SetUpsert(true),
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
// ReturnDocument(After) returns the document as it looks after the update. (DumboDBFull)
func TestFindOneAndUpdate_returnAfter(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
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
// ErrNoDocuments when the filter does not match any document. (DumboDBFull)
func TestFindOneAndReplace_no_match(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
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
// documents using dot-notation for nested fields. (DumboDBFull)
func TestCountDocuments_nested_filter(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
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
// 0 for an empty collection. (DumboDBFull)
func TestEstimatedDocumentCount_empty(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Collection has no documents; estimated count must be 0.
	count, err := coll.EstimatedDocumentCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "EstimatedDocumentCount on empty collection must return 0")
}
