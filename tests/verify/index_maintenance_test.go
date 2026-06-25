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

package verify

// TestIndexMaintenanceVerify is the automated analog of
// docs/verify/index-maintenance.md. Each top-level subtest corresponds
// to one scenario in that document; they run sequentially against one
// database so side effects carry forward exactly as the manual steps
// do.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// idxvCount runs the count command (the indexed-count fast path).
func idxvCount(t *testing.T, db *mongo.Database, coll string, query bson.D) int32 {
	t.Helper()
	var res bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "count", Value: coll},
		{Key: "query", Value: query},
	}).Decode(&res))
	switch n := res["n"].(type) {
	case int32:
		return n
	case int64:
		return int32(n)
	}
	t.Fatalf("count response missing n: %v", res)
	return 0
}

func idxvFindIDs(t *testing.T, db *mongo.Database, coll string, filter bson.D) []int32 {
	t.Helper()
	ctx := context.Background()
	cur, err := db.Collection(coll).Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "_id", Value: int32(1)}}))
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := []int32{}
	for _, d := range docs {
		ids = append(ids, d["_id"].(int32))
	}
	return ids
}

func TestIndexMaintenanceVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("idxmntvrfy%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	// Setup block from the doc.
	items := db.Collection("items")
	_, err := items.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}, {Key: "city", Value: "NYC"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "bravo"}, {Key: "city", Value: "LA"}},
		bson.D{{Key: "_id", Value: int32(3)}, {Key: "name", Value: "charlie"}, {Key: "city", Value: "NYC"}},
	})
	require.NoError(t, err)
	_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
	})
	require.NoError(t, err)
	_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
	})
	require.NoError(t, err)

	t.Run("Scenario1_UpdateReindexesChangedField", func(t *testing.T) {
		_, err := items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "zulu"}}}})
		require.NoError(t, err)

		assert.Equal(t, []int32{1}, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "zulu"}}))
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "alpha"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "zulu"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "alpha"}}))

		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "name", Value: "zulu"}})
		assert.Equal(t, "by_name", idxvIxscanName(wp), "re-indexed value must be served by the index: %v", wp)
	})

	t.Run("Scenario2_UpdateManyReindexesEveryDoc", func(t *testing.T) {
		_, err := items.UpdateMany(ctx,
			bson.D{{Key: "city", Value: "NYC"}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "city", Value: "SF"}}}})
		require.NoError(t, err)

		assert.EqualValues(t, 2, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "SF"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "NYC"}}))
	})

	t.Run("Scenario3_DeleteRemovesIndexEntries", func(t *testing.T) {
		_, err := items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, err)
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "bravo"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "bravo"}}))

		_, err = items.DeleteMany(ctx, bson.D{{Key: "city", Value: "SF"}})
		require.NoError(t, err)
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "SF"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{}))
	})

	t.Run("Scenario4_MultikeyUpdateAdjustsPerElement", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "tags", Value: bson.A{"red", "green", "blue"}}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "tags", Value: bson.A{"red"}}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "tags", Value: int32(1)}}, Options: options.Index().SetName("by_tags"),
		})
		require.NoError(t, err)

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(10)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "tags.1", Value: "yellow"}}}})
		require.NoError(t, err)

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "yellow"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "green"}}))
		assert.EqualValues(t, 2, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "red"}}))

		rangeFilter := bson.D{{Key: "tags", Value: bson.D{{Key: "$gt", Value: "a"}}}}
		assert.Equal(t, []int32{10, 11}, idxvFindIDs(t, db, "items", rangeFilter),
			"multi-element doc must be returned exactly once")
		assert.EqualValues(t, 2, idxvCount(t, db, "items", rangeFilter))

		eqPlan := idxvWinningPlan(t, db, "items", bson.D{{Key: "tags", Value: "yellow"}})
		assert.Equal(t, "by_tags", idxvIxscanName(eqPlan), "equality lookup must use by_tags: %v", eqPlan)
		rangePlan := idxvWinningPlan(t, db, "items", rangeFilter)
		assert.Equal(t, "by_tags", idxvIxscanName(rangePlan), "range lookup must use by_tags: %v", rangePlan)
	})

	t.Run("Scenario5_SparseIndexTracksFieldPresence", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "phone", Value: "555-0100"}},
			bson.D{{Key: "_id", Value: int32(21)}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "phone", Value: int32(1)}},
			Options: options.Index().SetName("by_phone").SetSparse(true),
		})
		require.NoError(t, err)

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(21)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "phone", Value: "555-0200"}}}})
		require.NoError(t, err)
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0200"}}))

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}},
			bson.D{{Key: "$unset", Value: bson.D{{Key: "phone", Value: ""}}}})
		require.NoError(t, err)
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))

		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "phone", Value: "555-0200"}})
		assert.Equal(t, "by_phone", idxvIxscanName(wp), "sparse index must serve equality lookups: %v", wp)
	})

	t.Run("Scenario6_PartialIndexTracksFilter", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(30)}, {Key: "sku", Value: "A-1"}, {Key: "status", Value: "active"}},
			bson.D{{Key: "_id", Value: int32(31)}, {Key: "sku", Value: "B-2"}, {Key: "status", Value: "inactive"}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "sku", Value: int32(1)}},
			Options: options.Index().SetName("by_sku_partial").
				SetPartialFilterExpression(bson.D{{Key: "status", Value: "active"}}),
		})
		require.NoError(t, err)

		coveredA1 := bson.D{{Key: "sku", Value: "A-1"}, {Key: "status", Value: "active"}}
		coveredB2 := bson.D{{Key: "sku", Value: "B-2"}, {Key: "status", Value: "active"}}
		skuOnlyA1 := bson.D{{Key: "sku", Value: "A-1"}}

		// Covered query (sku + partial condition) uses the index; sku-only
		// is declined to a scan (using the index would miss inactive docs).
		wpCovered := idxvWinningPlan(t, db, "items", coveredA1)
		assert.Equal(t, "by_sku_partial", idxvIxscanName(wpCovered),
			"covered query must use the partial index: %v", wpCovered)
		assert.Equal(t, []int32{30}, idxvFindIDs(t, db, "items", coveredA1))

		wpUncovered := idxvWinningPlan(t, db, "items", skuOnlyA1)
		assert.Equal(t, "COLLSCAN", wpUncovered["stage"],
			"sku-only query must be declined to a scan: %v", wpUncovered)

		// Flip membership: 30 leaves the filter, 31 enters it.
		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(30)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "inactive"}}}})
		require.NoError(t, err)
		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(31)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "active"}}}})
		require.NoError(t, err)

		// The index-using query reflects the new membership.
		assert.Empty(t, idxvFindIDs(t, db, "items", coveredA1),
			"no active A-1 doc remains after the flip")
		wpB2 := idxvWinningPlan(t, db, "items", coveredB2)
		assert.Equal(t, "by_sku_partial", idxvIxscanName(wpB2),
			"covered B-2 query must use the partial index: %v", wpB2)
		assert.Equal(t, []int32{31}, idxvFindIDs(t, db, "items", coveredB2))

		// The document is not gone -- a sku-only scan still finds A-1.
		assert.Equal(t, []int32{30}, idxvFindIDs(t, db, "items", skuOnlyA1))
	})

	// Scenario 7: cherry-picking an index-creation commit builds the index over
	// the TARGET branch's own documents (not the source branch's); a later merge
	// of the two branches is conflict-free and the index covers every document.
	t.Run("Scenario7_CherryPickIndexBuildThenMergeUnions", func(t *testing.T) {
		cpDB := fmt.Sprintf("idxmntcp%d", rand.Int64N(1_000_000))
		mainDB := env.Client.Database(cpDB + "@main")
		require.NoError(t, env.Client.Database(cpDB).Drop(ctx))

		// Baseline: a seed doc with no "name" field (so it never matches the
		// by_name value queries below). It is the common ancestor of both
		// branches; main's and feature's real documents are added after branching.
		_, err := mainDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(0)}, {Key: "tag", Value: "seed"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cpDB, "base seed", "alice <alice@acme.com>")
		idxvBranch(t, env, cpDB, "feature")

		// main: a few documents, then an index over them in a separate commit.
		_, err = mainDB.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "bravo"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, cpDB, "main: docs", "alice <alice@acme.com>")
		_, err = mainDB.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err)
		idxCommit := dumboDBCommit(t, env, cpDB, "main: create by_name", "alice <alice@acme.com>")

		// feature: different documents (no index yet).
		featDB := env.Client.Database(cpDB + "@feature")
		_, err = featDB.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "name", Value: "november"}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "name", Value: "oscar"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, cpDB+"@feature", "feature: docs", "bob <bob@widgets.io>")

		// Cherry-pick main's index-creation commit onto feature: must succeed and
		// build the index over feature's documents.
		cp := runCommandRaw(t, featDB, bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: idxCommit}})
		assert.EqualValues(t, 1, cp["ok"], "cherry-pick of index creation must succeed: %v", cp)

		// feature's index holds feature's docs only; main's docs are not here.
		assert.Equal(t, []int32{10}, idxvFindIDs(t, featDB, "items", bson.D{{Key: "name", Value: "november"}}))
		assert.Equal(t, []int32{11}, idxvFindIDs(t, featDB, "items", bson.D{{Key: "name", Value: "oscar"}}))
		assert.Empty(t, idxvFindIDs(t, featDB, "items", bson.D{{Key: "name", Value: "alpha"}}), "main's docs are not on feature")
		assert.EqualValues(t, 3, idxvCount(t, featDB, "items", bson.D{}), "feature has the seed + its own two docs")
		wp := idxvWinningPlan(t, featDB, "items", bson.D{{Key: "name", Value: "november"}})
		assert.Equal(t, "by_name", idxvIxscanName(wp), "feature lookup served by the cherry-picked index: %v", wp)

		// Merge feature into main: distinct docs, same index -> a clean 3-way merge.
		merged := idxvMerge(t, env, cpDB, "feature")
		assert.EqualValues(t, 1, merged["ok"], "merge must be clean (no conflicts): %v", merged)
		assert.NotEqual(t, "fast-forward", merged["message"], "must be a real 3-way merge")

		// Every document (main + feature) is now in the index.
		assert.Equal(t, []int32{1}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "alpha"}}))
		assert.Equal(t, []int32{2}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "bravo"}}))
		assert.Equal(t, []int32{10}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "november"}}))
		assert.Equal(t, []int32{11}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "oscar"}}))
		assert.EqualValues(t, 5, idxvCount(t, mainDB, "items", bson.D{}), "seed + main(2) + feature(2) present after merge")
		wpM := idxvWinningPlan(t, mainDB, "items", bson.D{{Key: "name", Value: "oscar"}})
		assert.Equal(t, "by_name", idxvIxscanName(wpM), "merged lookup served by the index: %v", wpM)
	})

	// Scenario 8: each branch creates a distinct index over the same documents
	// (main on the first field, feature on the second). Merging unions both the
	// documents and the index definitions; afterwards both indexes cover every
	// document from both branches.
	t.Run("Scenario8_DistinctIndexesPerBranchMergeUnions", func(t *testing.T) {
		twoDB := fmt.Sprintf("idxmnt2idx%d", rand.Int64N(1_000_000))
		mainDB := env.Client.Database(twoDB + "@main")
		require.NoError(t, env.Client.Database(twoDB).Drop(ctx))

		// Baseline: one document with both fields, the common ancestor.
		_, err := mainDB.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: int32(0)}, {Key: "name", Value: "seed"}, {Key: "city", Value: "Origin"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, twoDB, "base seed", "alice <alice@acme.com>")
		idxvBranch(t, env, twoDB, "feature")

		// main: a few docs, then an index over the first field (name).
		_, err = mainDB.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}, {Key: "city", Value: "NYC"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "bravo"}, {Key: "city", Value: "LA"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, twoDB, "main: docs", "alice <alice@acme.com>")
		_, err = mainDB.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, twoDB, "main: create by_name", "alice <alice@acme.com>")

		// feature: different docs, then an index over the second field (city).
		featDB := env.Client.Database(twoDB + "@feature")
		_, err = featDB.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "name", Value: "november"}, {Key: "city", Value: "Boston"}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "name", Value: "oscar"}, {Key: "city", Value: "Denver"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, twoDB+"@feature", "feature: docs", "bob <bob@widgets.io>")
		_, err = featDB.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, twoDB+"@feature", "feature: create by_city", "bob <bob@widgets.io>")

		// Merge feature into main: distinct docs and distinct indexes -> clean merge.
		merged := idxvMerge(t, env, twoDB, "feature")
		assert.EqualValues(t, 1, merged["ok"], "merge must be clean (no conflicts): %v", merged)
		assert.NotEqual(t, "fast-forward", merged["message"], "must be a real 3-way merge")

		assert.EqualValues(t, 5, idxvCount(t, mainDB, "items", bson.D{}), "seed + main(2) + feature(2) present after merge")

		// by_name (created on main) now covers every document, including feature's.
		assert.Equal(t, []int32{0}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "seed"}}))
		assert.Equal(t, []int32{1}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "alpha"}}))
		assert.Equal(t, []int32{10}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "november"}}))
		assert.Equal(t, []int32{11}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "name", Value: "oscar"}}))
		wpName := idxvWinningPlan(t, mainDB, "items", bson.D{{Key: "name", Value: "november"}})
		assert.Equal(t, "by_name", idxvIxscanName(wpName), "by_name must serve feature's docs after merge: %v", wpName)

		// by_city (created on feature) now covers every document, including main's.
		assert.Equal(t, []int32{0}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "city", Value: "Origin"}}))
		assert.Equal(t, []int32{1}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "city", Value: "NYC"}}))
		assert.Equal(t, []int32{2}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "city", Value: "LA"}}))
		assert.Equal(t, []int32{10}, idxvFindIDs(t, mainDB, "items", bson.D{{Key: "city", Value: "Boston"}}))
		wpCity := idxvWinningPlan(t, mainDB, "items", bson.D{{Key: "city", Value: "NYC"}})
		assert.Equal(t, "by_city", idxvIxscanName(wpCity), "by_city must serve main's docs after merge: %v", wpCity)
	})
}
