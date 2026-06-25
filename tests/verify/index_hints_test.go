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

// TestIndexHintsVerify is the automated analog of
// docs/verify/index-hints.md. Each subtest corresponds to one scenario
// in that document.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// idxvWinningPlanHinted returns the winningPlan for an explained find with a
// hint applied.
func idxvWinningPlanHinted(t *testing.T, db *mongo.Database, coll string, filter bson.D, hint any) bson.M {
	t.Helper()
	var res bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "find", Value: coll},
			{Key: "filter", Value: filter},
			{Key: "hint", Value: hint},
		}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&res))
	qp, ok := res["queryPlanner"].(bson.M)
	require.True(t, ok, "explain response missing queryPlanner: %v", res)
	wp, ok := qp["winningPlan"].(bson.M)
	require.True(t, ok, "queryPlanner missing winningPlan: %v", qp)
	return wp
}

// idxvHintErrorCode runs find(filter).hint(hint) and returns the resulting
// command error code, or 0 if the query succeeded.
func idxvHintErrorCode(t *testing.T, db *mongo.Database, coll string, filter bson.D, hint any) int32 {
	t.Helper()
	cur, err := db.Collection(coll).Find(context.Background(), filter, options.Find().SetHint(hint))
	if err == nil {
		var out []bson.D
		err = cur.All(context.Background(), &out)
	}
	if err == nil {
		return 0
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return int32(cmdErr.Code)
	}
	t.Fatalf("expected a command error, got %T: %v", err, err)
	return 0
}

func TestIndexHintsVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("idxhintvrfy%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

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

	bothFields := bson.D{{Key: "name", Value: "alpha"}, {Key: "city", Value: "NYC"}}

	t.Run("Scenario1_HintForcesIndexByName", func(t *testing.T) {
		unhinted := idxvWinningPlan(t, db, "items", bothFields)
		assert.Equal(t, "by_name", idxvIxscanName(unhinted),
			"default plan should use by_name: %v", unhinted)

		hinted := idxvWinningPlanHinted(t, db, "items", bothFields, "by_city")
		assert.Equal(t, "by_city", idxvIxscanName(hinted),
			"hint must force by_city: %v", hinted)

		assert.Equal(t, []int32{1}, idxvFindIDs(t, db, "items", bothFields))
	})

	t.Run("Scenario2_HintByKeyPattern", func(t *testing.T) {
		hinted := idxvWinningPlanHinted(t, db, "items", bothFields, bson.D{{Key: "city", Value: int32(1)}})
		assert.Equal(t, "by_city", idxvIxscanName(hinted),
			"key-pattern hint must resolve to by_city: %v", hinted)
	})

	t.Run("Scenario3_NaturalHintForcesCollscan", func(t *testing.T) {
		wp := idxvWinningPlanHinted(t, db, "items", bson.D{{Key: "city", Value: "NYC"}}, bson.D{{Key: "$natural", Value: int32(1)}})
		assert.Equal(t, "COLLSCAN", wp["stage"],
			"$natural must force a collection scan: %v", wp)
		assert.Equal(t, []int32{1, 3}, idxvFindIDs(t, db, "items", bson.D{{Key: "city", Value: "NYC"}}))
	})

	t.Run("Scenario4_NonExistentHintErrors", func(t *testing.T) {
		// BadValue is code 2.
		assert.EqualValues(t, 2, idxvHintErrorCode(t, db, "items", bson.D{{Key: "city", Value: "NYC"}}, "no_such_index"))
	})

	t.Run("Scenario5_NonCoveringHintReturnsCorrect", func(t *testing.T) {
		cur, err := items.Find(ctx, bson.D{{Key: "city", Value: "NYC"}},
			options.Find().SetHint("by_name").SetSort(bson.D{{Key: "_id", Value: int32(1)}}))
		require.NoError(t, err)
		var got []bson.M
		require.NoError(t, cur.All(ctx, &got))
		ids := make([]int32, 0, len(got))
		for _, d := range got {
			ids = append(ids, d["_id"].(int32))
		}
		assert.Equal(t, []int32{1, 3}, ids,
			"non-covering hint must still return the matching docs")
	})
}
