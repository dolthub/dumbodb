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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestIndex_TTL_CreateOne verifies that a TTL index (expireAfterSeconds) can be created (do-81xd).
func TestIndex_TTL_CreateOne(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(3600)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: &options.IndexOptions{ExpireAfterSeconds: &expireAfter},
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "createdAt_1", name)
}

// TestIndex_TTL_ZeroSeconds verifies TTL index with expireAfterSeconds=0 (do-81xd).
func TestIndex_TTL_ZeroSeconds(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(0)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "expireAt", Value: 1}},
		Options: &options.IndexOptions{ExpireAfterSeconds: &expireAfter},
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "expireAt_1", name)
}

// TestIndex_TTL_InsertDocs verifies that inserts work on a TTL-indexed collection (do-81xd).
func TestIndex_TTL_InsertDocs(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(3600)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "ts", Value: 1}},
		Options: &options.IndexOptions{ExpireAfterSeconds: &expireAfter},
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "ts", Value: time.Now()},
		{Key: "data", Value: "fresh"},
	})
	require.NoError(t, err)

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_Partial_CreateOne verifies that a partial index can be created (do-81xd).
func TestIndex_Partial_CreateOne(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	filter := bson.D{{Key: "score", Value: bson.D{{Key: "$gt", Value: int32(10)}}}}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "score", Value: 1}},
		Options: options.Index().SetPartialFilterExpression(filter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "score_1", name)
}

// TestIndex_Partial_WithExistsFilter verifies partial index with $exists filter (do-81xd).
func TestIndex_Partial_WithExistsFilter(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	filter := bson.D{{Key: "email", Value: bson.D{{Key: "$exists", Value: true}}}}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetPartialFilterExpression(filter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "email_1", name)
}

// TestIndex_Collation_CreateOne verifies that a collation index can be created (do-81xd).
func TestIndex_Collation_CreateOne(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	collation := options.Collation{Locale: "en", Strength: 2}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "username", Value: 1}},
		Options: options.Index().SetCollation(&collation),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "username_1", name)
}

// TestIndex_WildcardProjection_CreateOne verifies wildcard index with projection (do-81xd).
func TestIndex_WildcardProjection_CreateOne(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{
		Keys: bson.D{{Key: "$**", Value: 1}},
		Options: options.Index().SetWildcardProjection(bson.D{
			{Key: "a", Value: int32(1)},
			{Key: "b", Value: int32(1)},
		}),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "$**_1", name)
}

// TestIndex_Hashed_CreateOne verifies that a hashed index can be created (do-81xd).
func TestIndex_Hashed_CreateOne(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: "hashed"}}}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "user_id_hashed", name)
}

// TestIndex_Hashed_EqualityQuery verifies that a hashed index allows equality queries (do-81xd).
func TestIndex_Hashed_EqualityQuery(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "uid", Value: "hashed"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "uid", Value: "u1"}, {Key: "val", Value: 1}},
		bson.D{{Key: "uid", Value: "u2"}, {Key: "val", Value: 2}},
	)

	count, err := coll.CountDocuments(ctx, bson.D{{Key: "uid", Value: "u1"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_Hashed_CannotBeUnique verifies that hashed+unique returns an error (do-81xd).
func TestIndex_Hashed_CannotBeUnique(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	unique := true
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "shardKey", Value: "hashed"}},
		Options: &options.IndexOptions{Unique: &unique},
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.Error(t, err, "hashed+unique should fail")
}

// TestIndex_Sparse_UniqueWithMissingField verifies sparse+unique allows multiple docs
// without the indexed field (do-81xd).
func TestIndex_Sparse_UniqueWithMissingField(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	sparse := true
	unique := true
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: &options.IndexOptions{Sparse: &sparse, Unique: &unique},
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	// Both docs lack the email field — sparse+unique should allow this.
	_, err = coll.InsertOne(ctx, bson.D{{Key: "name", Value: "A"}})
	require.NoError(t, err)
	_, err = coll.InsertOne(ctx, bson.D{{Key: "name", Value: "B"}})
	require.NoError(t, err)

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestIndex_ListIndexes_AfterDrop verifies that listIndexes returns 1 after
// dropping the only secondary index (do-81xd).
func TestIndex_ListIndexes_AfterDrop(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "x", Value: 1}}}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	_, err = coll.Indexes().DropOne(ctx, name)
	require.NoError(t, err)

	cur, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.D
	require.NoError(t, cur.All(ctx, &indexes))
	require.Equal(t, 1, len(indexes), "only _id_ index should remain")
}

// TestIndex_ListIndexes_AfterDropAll verifies that listIndexes returns 1 after DropAll (do-81xd).
func TestIndex_ListIndexes_AfterDropAll(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	models := []mongo.IndexModel{
		{Keys: bson.D{{Key: "x", Value: 1}}},
		{Keys: bson.D{{Key: "y", Value: -1}}},
	}
	_, err := coll.Indexes().CreateMany(ctx, models)
	require.NoError(t, err)

	_, err = coll.Indexes().DropAll(ctx)
	require.NoError(t, err)

	cur, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.D
	require.NoError(t, cur.All(ctx, &indexes))
	require.Equal(t, 1, len(indexes), "only _id_ index should remain after DropAll")
}

// TestIndex_IndexStats_Basic verifies that $indexStats returns one doc per index (do-81xd).
func TestIndex_IndexStats_Basic(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "score", Value: 1}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "score", Value: int32(1)}},
		bson.D{{Key: "score", Value: int32(2)}},
	)

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$indexStats", Value: bson.D{}}},
	}
	cur, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var stats []bson.D
	require.NoError(t, cur.All(ctx, &stats))
	// _id_ + score_1 = 2 indexes
	require.Equal(t, 2, len(stats))
}

// TestIndex_IndexStats_NoIndexes verifies $indexStats returns 1 doc (just _id_) for a
// collection with no secondary indexes (do-81xd).
func TestIndex_IndexStats_NoIndexes(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a doc to ensure the collection exists.
	insertDocs(t, coll, bson.D{{Key: "x", Value: 1}})

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$indexStats", Value: bson.D{}}},
	}
	cur, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var stats []bson.D
	require.NoError(t, cur.All(ctx, &stats))
	// Only _id_ index
	require.Equal(t, 1, len(stats))
}
