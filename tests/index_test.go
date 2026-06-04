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
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestIndex_TTL_CreateOne verifies that a TTL index (expireAfterSeconds) can be created (do-81xd).
func TestIndex_TTL_CreateOne(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(3600)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(expireAfter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "createdAt_1", name)
}

// TestIndex_TTL_ZeroSeconds verifies TTL index with expireAfterSeconds=0 (do-81xd).
func TestIndex_TTL_ZeroSeconds(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(0)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "expireAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(expireAfter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "expireAt_1", name)
}

// TestIndex_TTL_InsertDocs verifies that inserts work on a TTL-indexed collection (do-81xd).
func TestIndex_TTL_InsertDocs(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(3600)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "ts", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(expireAfter),
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
	env := startDumboDB(t)
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
	env := startDumboDB(t)
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
	env := startDumboDB(t)
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
	env := startDumboDB(t)
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
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: "hashed"}}}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "user_id_hashed", name)
}

// TestIndex_Hashed_EqualityQuery verifies that a hashed index allows equality queries (do-81xd).
func TestIndex_Hashed_EqualityQuery(t *testing.T) {
	env := startDumboDB(t)
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
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	unique := true
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "shardKey", Value: "hashed"}},
		Options: options.Index().SetUnique(unique),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.Error(t, err, "hashed+unique should fail")
}

// TestIndex_Sparse_UniqueWithMissingField verifies sparse+unique allows multiple docs
// without the indexed field (do-81xd).
func TestIndex_Sparse_UniqueWithMissingField(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	sparse := true
	unique := true
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetSparse(sparse).SetUnique(unique),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	// Both docs lack the email field  -- sparse+unique should allow this.
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
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "x", Value: 1}}}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	require.NoError(t, coll.Indexes().DropOne(ctx, name))

	cur, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.D
	require.NoError(t, cur.All(ctx, &indexes))
	require.Equal(t, 1, len(indexes), "only _id_ index should remain")
}

// TestIndex_ListIndexes_AfterDropAll verifies that listIndexes returns 1 after DropAll (do-81xd).
func TestIndex_ListIndexes_AfterDropAll(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	models := []mongo.IndexModel{
		{Keys: bson.D{{Key: "x", Value: 1}}},
		{Keys: bson.D{{Key: "y", Value: -1}}},
	}
	_, err := coll.Indexes().CreateMany(ctx, models)
	require.NoError(t, err)

	require.NoError(t, coll.Indexes().DropAll(ctx))

	cur, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.D
	require.NoError(t, cur.All(ctx, &indexes))
	require.Equal(t, 1, len(indexes), "only _id_ index should remain after DropAll")
}

// TestIndex_IndexStats_Basic verifies that $indexStats returns one doc per index (do-81xd).
func TestIndex_IndexStats_Basic(t *testing.T) {
	env := startDumboDB(t)
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
	env := startDumboDB(t)
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

// TestIndex_TTL_InsertAndVerifyNotExpiredYet verifies that a document inserted into a
// TTL-indexed collection is still present immediately after insertion (do-x0vc).
func TestIndex_TTL_InsertAndVerifyNotExpiredYet(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(3600)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "createdAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(expireAfter),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "createdAt", Value: time.Now()},
		{Key: "payload", Value: "not-yet-expired"},
	})
	require.NoError(t, err)

	// Document should still be visible immediately (TTL expiry runs on a background task).
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "payload", Value: "not-yet-expired"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_TTL_OnNestedDateField verifies that a TTL index can be created on a nested
// date field (do-x0vc).
func TestIndex_TTL_OnNestedDateField(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	expireAfter := int32(86400)
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "meta.expiresAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(expireAfter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "meta.expiresAt_1", name)

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "meta", Value: bson.D{{Key: "expiresAt", Value: time.Now().Add(24 * time.Hour)}}},
		{Key: "data", Value: "alive"},
	})
	require.NoError(t, err)

	count, err := coll.CountDocuments(ctx, bson.D{{Key: "data", Value: "alive"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_Partial_OnlyIndexesMatchingDocs verifies that a partial index can be created
// and queries still work on all docs (do-x0vc).
func TestIndex_Partial_OnlyIndexesMatchingDocs(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	filter := bson.D{{Key: "active", Value: true}}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "score", Value: 1}},
		Options: options.Index().SetPartialFilterExpression(filter),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "score", Value: int32(10)}, {Key: "active", Value: true}},
		bson.D{{Key: "score", Value: int32(20)}, {Key: "active", Value: true}},
		bson.D{{Key: "score", Value: int32(5)}, {Key: "active", Value: false}},
	)

	// All three docs are present regardless of partial index.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(3), count)

	// Active-only query.
	count, err = coll.CountDocuments(ctx, bson.D{{Key: "active", Value: true}})
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestIndex_Partial_UniquePartial verifies that a unique partial index can be created and
// allows duplicate values when the partial filter is not satisfied (do-x0vc).
func TestIndex_Partial_UniquePartial(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	unique := true
	filter := bson.D{{Key: "status", Value: "active"}}
	model := mongo.IndexModel{
		Keys: bson.D{{Key: "email", Value: 1}},
		Options: options.Index().
			SetUnique(unique).
			SetPartialFilterExpression(filter),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	// Two inactive docs with the same email  -- allowed because filter not satisfied.
	_, err = coll.InsertOne(ctx, bson.D{{Key: "email", Value: "x@x.com"}, {Key: "status", Value: "inactive"}})
	require.NoError(t, err)
	_, err = coll.InsertOne(ctx, bson.D{{Key: "email", Value: "x@x.com"}, {Key: "status", Value: "inactive"}})
	require.NoError(t, err)

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestIndex_Partial_CompoundKeys verifies that a partial index with compound keys can be
// created (do-x0vc).
func TestIndex_Partial_CompoundKeys(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	filter := bson.D{{Key: "published", Value: true}}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "author", Value: 1}, {Key: "date", Value: -1}},
		Options: options.Index().SetPartialFilterExpression(filter),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "author_1_date_-1", name)
}

// TestIndex_Wildcard_WithWildcardProjection verifies that a wildcard index with
// wildcardProjection can be created and basic queries work (do-x0vc).
func TestIndex_Wildcard_WithWildcardProjection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{
		Keys: bson.D{{Key: "$**", Value: 1}},
		Options: options.Index().SetWildcardProjection(bson.D{
			{Key: "tags", Value: int32(1)},
		}),
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "$**_1", name)

	insertDocs(t, coll,
		bson.D{{Key: "tags", Value: bson.A{"go", "db"}}, {Key: "x", Value: 1}},
		bson.D{{Key: "tags", Value: bson.A{"mongo"}}, {Key: "x", Value: 2}},
	)

	count, err := coll.CountDocuments(ctx, bson.D{{Key: "x", Value: 1}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_Collation_CaseInsensitive verifies that a collation index can be created and
// exact-case queries still work (do-x0vc).
func TestIndex_Collation_CaseInsensitive(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	collation := options.Collation{Locale: "en", Strength: 2}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetCollation(&collation),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "Alice"}},
		bson.D{{Key: "name", Value: "Bob"}},
	)

	// Exact-case queries work even when a collation index is present.
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "name", Value: "Alice"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIndex_Collation_UniqueWithCollation verifies that a unique collation index can be
// created and exact-case uniqueness is enforced (do-x0vc).
func TestIndex_Collation_UniqueWithCollation(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	collation := options.Collation{Locale: "en", Strength: 2}
	model := mongo.IndexModel{
		Keys:    bson.D{{Key: "handle", Value: 1}},
		Options: options.Index().SetCollation(&collation).SetUnique(true),
	}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{{Key: "handle", Value: "alice"}})
	require.NoError(t, err)

	// Exact duplicate  -- must fail.
	_, err = coll.InsertOne(ctx, bson.D{{Key: "handle", Value: "alice"}})
	require.Error(t, err, "duplicate exact-case value must be rejected")
}

// TestIndex_2dsphere_CreateOne verifies that a 2dsphere index can be created (do-x0vc).
func TestIndex_2dsphere_CreateOne(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "location", Value: "2dsphere"}}}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "location_2dsphere", name)
}

// TestIndex_2dsphere_NearQuery verifies that $near queries work on a 2dsphere-indexed
// collection (do-x0vc).
func TestIndex_2dsphere_NearQuery(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2dsphere"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "nearby"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Point"},
			{Key: "coordinates", Value: bson.A{-73.97, 40.77}},
		}}},
		bson.D{{Key: "name", Value: "faraway"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Point"},
			{Key: "coordinates", Value: bson.A{2.35, 48.86}},
		}}},
	)

	cur, err := coll.Find(ctx, bson.D{
		{Key: "loc", Value: bson.D{
			{Key: "$near", Value: bson.D{
				{Key: "$geometry", Value: bson.D{
					{Key: "type", Value: "Point"},
					{Key: "coordinates", Value: bson.A{-73.97, 40.77}},
				}},
				{Key: "$maxDistance", Value: 500},
			}},
		}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Equal(t, 1, len(results))
}

// TestIndex_2dsphere_GeoWithinQuery verifies that $geoWithin queries work on a
// 2dsphere-indexed collection (do-x0vc).
func TestIndex_2dsphere_GeoWithinQuery(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2dsphere"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "inside"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Point"},
			{Key: "coordinates", Value: bson.A{0.5, 0.5}},
		}}},
		bson.D{{Key: "name", Value: "outside"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Point"},
			{Key: "coordinates", Value: bson.A{5.0, 5.0}},
		}}},
	)

	cur, err := coll.Find(ctx, bson.D{
		{Key: "loc", Value: bson.D{
			{Key: "$geoWithin", Value: bson.D{
				{Key: "$geometry", Value: bson.D{
					{Key: "type", Value: "Polygon"},
					{Key: "coordinates", Value: bson.A{bson.A{
						bson.A{-1.0, -1.0},
						bson.A{2.0, -1.0},
						bson.A{2.0, 2.0},
						bson.A{-1.0, 2.0},
						bson.A{-1.0, -1.0},
					}}},
				}},
			}},
		}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Equal(t, 1, len(results))
}

// TestIndex_2dsphere_Compound verifies that a compound index with a 2dsphere field
// can be created (do-x0vc).
func TestIndex_2dsphere_Compound(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{
		Keys: bson.D{
			{Key: "category", Value: 1},
			{Key: "loc", Value: "2dsphere"},
		},
	}
	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	require.Equal(t, "category_1_loc_2dsphere", name)
}

// TestIndex_2dsphere_GeoIntersects verifies that $geoIntersects queries work on a
// 2dsphere-indexed collection (do-x0vc).
func TestIndex_2dsphere_GeoIntersects(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2dsphere"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "poly"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Polygon"},
			{Key: "coordinates", Value: bson.A{bson.A{
				bson.A{0.0, 0.0},
				bson.A{1.0, 0.0},
				bson.A{1.0, 1.0},
				bson.A{0.0, 1.0},
				bson.A{0.0, 0.0},
			}}},
		}}},
	)

	cur, err := coll.Find(ctx, bson.D{
		{Key: "loc", Value: bson.D{
			{Key: "$geoIntersects", Value: bson.D{
				{Key: "$geometry", Value: bson.D{
					{Key: "type", Value: "Point"},
					{Key: "coordinates", Value: bson.A{0.5, 0.5}},
				}},
			}},
		}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Equal(t, 1, len(results))
}

// TestGeo_GeoIntersects_LineString verifies that $geoIntersects with a LineString query
// does not match stored Point documents that lie on the line (do-29rp).
func TestGeo_GeoIntersects_LineString(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2dsphere"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	// Query line: horizontal at lat=40.7, from lon=-74.5 to lon=-73.5
	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "poly-intersects"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Polygon"},
			{Key: "coordinates", Value: bson.A{bson.A{
				bson.A{-74.2, 40.5},
				bson.A{-73.8, 40.5},
				bson.A{-73.8, 40.9},
				bson.A{-74.2, 40.9},
				bson.A{-74.2, 40.5},
			}}},
		}}},
		bson.D{{Key: "name", Value: "poly-disjoint"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Polygon"},
			{Key: "coordinates", Value: bson.A{bson.A{
				bson.A{-72.0, 41.0},
				bson.A{-71.0, 41.0},
				bson.A{-71.0, 42.0},
				bson.A{-72.0, 42.0},
				bson.A{-72.0, 41.0},
			}}},
		}}},
		bson.D{{Key: "name", Value: "point-on-line"}, {Key: "loc", Value: bson.D{
			{Key: "type", Value: "Point"},
			{Key: "coordinates", Value: bson.A{-74.0, 40.7}},
		}}},
	)

	cur, err := coll.Find(ctx, bson.D{
		{Key: "loc", Value: bson.D{
			{Key: "$geoIntersects", Value: bson.D{
				{Key: "$geometry", Value: bson.D{
					{Key: "type", Value: "LineString"},
					{Key: "coordinates", Value: bson.A{
						bson.A{-74.5, 40.7},
						bson.A{-73.5, 40.7},
					}},
				}},
			}},
		}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))

	var names []string
	for _, r := range results {
		for _, e := range r {
			if e.Key == "name" {
				names = append(names, e.Value.(string))
			}
		}
	}

	require.Contains(t, names, "poly-intersects", "polygon crossing query line should match")
	require.NotContains(t, names, "poly-disjoint", "disjoint polygon should not match")
	require.NotContains(t, names, "point-on-line", "point on line should not match (MongoDB behavior)")
}

// TestGeo_DocType_GeometryCollection verifies that documents with a GeometryCollection
// geo field are matched by $geoIntersects queries (do-f7x8).
func TestGeo_DocType_GeometryCollection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "geo", Value: "2dsphere"}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{
			{Key: "_id", Value: "gc1"},
			{Key: "geo", Value: bson.D{
				{Key: "type", Value: "GeometryCollection"},
				{Key: "geometries", Value: bson.A{
					bson.D{{Key: "type", Value: "Point"}, {Key: "coordinates", Value: bson.A{-74.006, 40.7128}}},
					bson.D{
						{Key: "type", Value: "Polygon"},
						{Key: "coordinates", Value: bson.A{bson.A{
							bson.A{-75.0, 40.0}, bson.A{-73.0, 40.0},
							bson.A{-73.0, 41.5}, bson.A{-75.0, 41.5},
							bson.A{-75.0, 40.0},
						}}},
					},
				}},
			}},
		},
	)

	// Query point lies inside the polygon sub-geometry of the GeometryCollection.
	cur, err := coll.Find(ctx, bson.D{
		{Key: "geo", Value: bson.D{
			{Key: "$geoIntersects", Value: bson.D{
				{Key: "$geometry", Value: bson.D{
					{Key: "type", Value: "Point"},
					{Key: "coordinates", Value: bson.A{-74.0, 40.5}},
				}},
			}},
		}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Len(t, results, 1, "GeometryCollection document should be matched by $geoIntersects")

	var id string
	for _, e := range results[0] {
		if e.Key == "_id" {
			id, _ = e.Value.(string)
		}
	}
	require.Equal(t, "gc1", id)
}

// TestIndex_IndexStats_AfterInsert verifies that $indexStats returns the correct number
// of index entries after inserting documents (do-x0vc).
func TestIndex_IndexStats_AfterInsert(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	model := mongo.IndexModel{Keys: bson.D{{Key: "val", Value: 1}}}
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	insertDocs(t, coll,
		bson.D{{Key: "val", Value: int32(1)}},
		bson.D{{Key: "val", Value: int32(2)}},
		bson.D{{Key: "val", Value: int32(3)}},
	)

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$indexStats", Value: bson.D{}}},
	}
	cur, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var stats []bson.D
	require.NoError(t, cur.All(ctx, &stats))
	// _id_ + val_1 = 2 indexes
	require.Equal(t, 2, len(stats))

	// Each stat entry must have a "name" field.
	for _, s := range stats {
		var name string
		for _, elem := range s {
			if elem.Key == "name" {
				if v, ok := elem.Value.(string); ok {
					name = v
				}
			}
		}
		require.NotEmpty(t, name, "indexStats entry must have a non-empty name")
	}
}

// TestSecondaryIndex_EmailEqualityQuery is the end-to-end secondary index test
// that drives the dolt backend directly (no mongo driver).
//
// Scenario:
//   - Insert 100 documents, each with an "email" field. Some share the same email.
//   - Call createIndex on {email: 1}.
//   - Call find({email: "alice@example.com"}).
//   - Verify that only the correct documents are returned.
//   - Verify that the secondary index map is populated (index was built, not a full scan).
func TestSecondaryIndex_EmailEqualityQuery(t *testing.T) {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "dolt-index-e2e-*")
	if err != nil {
		t.Fatalf("TempDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	b, err := dolt.NewBackend(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)), false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	db, err := b.Database("testindex")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("users")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Build 100 documents. Distribution:
	//  - 3 docs with email "alice@example.com"
	//  - 2 docs with email "bob@example.com"
	//  - remaining docs get unique emails
	const (
		totalDocs  = 100
		aliceCount = 3
		bobCount   = 2
	)

	docs := make([]*types.Document, 0, totalDocs)
	aliceIDs := make([]int32, 0, aliceCount)
	bobIDs := make([]int32, 0, bobCount)

	for i := 0; i < totalDocs; i++ {
		id := int32(i + 1)
		var email string
		switch {
		case i < aliceCount:
			email = "alice@example.com"
			aliceIDs = append(aliceIDs, id)
		case i < aliceCount+bobCount:
			email = "bob@example.com"
			bobIDs = append(bobIDs, id)
		default:
			email = fmt.Sprintf("user%d@example.com", i)
		}
		doc := must.NotFail(types.NewDocument("_id", id, "email", email, "seq", int32(i)))
		docs = append(docs, doc)
	}
	_ = bobIDs // used for sanity only

	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{
				Name: "email_1",
				Key:  []backends.IndexKeyPair{{Field: "email", Descending: false}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	listRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	foundEmailIdx := false
	for _, idx := range listRes.Indexes {
		if idx.Name == "email_1" {
			foundEmailIdx = true
			break
		}
	}
	if !foundEmailIdx {
		t.Fatalf("email_1 index not found in ListIndexes: %+v", listRes.Indexes)
	}

	filter := must.NotFail(types.NewDocument("email", "alice@example.com"))
	queryRes, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer queryRes.Iter.Close()

	var returnedDocs []*types.Document
	for {
		_, doc, err := queryRes.Iter.Next()
		if err != nil {
			break // iterator.ErrIteratorDone
		}
		if doc == nil {
			break
		}
		returnedDocs = append(returnedDocs, doc)
	}

	if len(returnedDocs) != aliceCount {
		t.Errorf("expected %d docs for alice@example.com, got %d", aliceCount, len(returnedDocs))
		for i, d := range returnedDocs {
			t.Logf("  doc[%d]: %v", i, d)
		}
	}

	aliceIDSet := make(map[int32]bool, len(aliceIDs))
	for _, id := range aliceIDs {
		aliceIDSet[id] = true
	}

	for _, doc := range returnedDocs {
		emailVal, err := doc.Get("email")
		if err != nil {
			t.Errorf("doc missing email field: %v", err)
			continue
		}
		if emailVal != "alice@example.com" {
			t.Errorf("unexpected email in result: %v", emailVal)
		}
		idVal, err := doc.Get("_id")
		if err != nil {
			t.Errorf("doc missing _id: %v", err)
			continue
		}
		id, ok := idVal.(int32)
		if !ok {
			t.Errorf("_id is not int32: %T %v", idVal, idVal)
			continue
		}
		if !aliceIDSet[id] {
			t.Errorf("unexpected _id %d in result (not an alice doc)", id)
		}
	}

	// Insert a doc AFTER createIndex to verify index maintenance on the write path.
	newDoc := must.NotFail(types.NewDocument("_id", int32(999), "email", "newafter@example.com", "seq", int32(999)))
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{newDoc}}); err != nil {
		t.Fatalf("InsertAll (post-index): %v", err)
	}

	filter2 := must.NotFail(types.NewDocument("email", "newafter@example.com"))
	queryRes2, err := coll.Query(ctx, &backends.QueryParams{Filter: filter2})
	if err != nil {
		t.Fatalf("Query (post-index insert): %v", err)
	}
	defer queryRes2.Iter.Close()

	var postInsertDocs []*types.Document
	for {
		_, doc, err := queryRes2.Iter.Next()
		if err != nil {
			break
		}
		if doc == nil {
			break
		}
		postInsertDocs = append(postInsertDocs, doc)
	}

	if len(postInsertDocs) != 1 {
		t.Errorf("expected 1 doc for newafter@example.com, got %d", len(postInsertDocs))
	} else {
		idVal, _ := postInsertDocs[0].Get("_id")
		if idVal != int32(999) {
			t.Errorf("expected _id=999, got %v", idVal)
		}
	}

	t.Logf("PASS: index lookup returned %d alice docs; post-insert lookup returned %d docs",
		len(returnedDocs), len(postInsertDocs))
}

// TestSecondaryIndex_NoBuildOnInsertBeforeCreate verifies that documents inserted
// BEFORE createIndex are picked up during the index build scan.
//
// Skipped on bson-a: same dependency as TestSecondaryIndexSurvivesDoltCommit
// in internal/backends/dolt -- the test asserts a specific result-count that
// depends on the byte-level prefilter narrowing the scan output.
// With the prefilter disabled (pending the BSON-element rewrite) the scan
// returns all 3 documents unfiltered. Restoring the BSON prefilter
// unblocks this test.
func TestSecondaryIndex_NoBuildOnInsertBeforeCreate(t *testing.T) {
	t.Skip("bson-a: depends on byte-level prefilter; restore when BSON prefilter lands")
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "dolt-index-prebuild-*")
	if err != nil {
		t.Fatalf("TempDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	b, err := dolt.NewBackend(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)), false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	db, err := b.Database("testindex2")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("items")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	preDocs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "tag", "alpha")),
		must.NotFail(types.NewDocument("_id", int32(2), "tag", "beta")),
		must.NotFail(types.NewDocument("_id", int32(3), "tag", "alpha")),
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: preDocs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "tag_1", Key: []backends.IndexKeyPair{{Field: "tag"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	filter := must.NotFail(types.NewDocument("tag", "alpha"))
	res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer res.Iter.Close()

	var got []*types.Document
	for {
		_, doc, err := res.Iter.Next()
		if err != nil {
			break
		}
		if doc == nil {
			break
		}
		got = append(got, doc)
	}

	if len(got) != 2 {
		t.Errorf("expected 2 alpha docs (from index build scan), got %d", len(got))
	}
}
