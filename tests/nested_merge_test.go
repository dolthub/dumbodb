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
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMergeMatrix_NestedDocument(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	for _, mode := range []string{"", "fieldTouched", "fieldDivergent", "documentTouched", "documentDivergent"} {
		name := mode
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			dbName := fmt.Sprintf("nested_%d", rand.Int64N(1_000_000))
			mainDB := env.Client.Database(dbName + "@main")
			create := bson.D{{Key: "create", Value: "docs"}}
			if mode != "" {
				create = append(create, bson.E{Key: "mergeMode", Value: mode})
			}
			require.NoError(t, mainDB.RunCommand(ctx, create).Err())
			_, err := mainDB.Collection("docs").InsertOne(ctx, bson.D{
				{Key: "_id", Value: int32(1)},
				{Key: "meta", Value: bson.D{{Key: "owner", Value: "alice"}, {Key: "region", Value: "eu"}}},
			})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@main", "seed")
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
			}).Err())
			featureDB := env.Client.Database(dbName + "@feature")
			_, err = featureDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "meta.region", Value: "us"}}}})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@feature", "region")
			_, err = mainDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "meta.owner", Value: "bob"}}}})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@main", "owner")
			err = mainDB.RunCommand(ctx, bson.D{{Key: "dumboMerge", Value: 1}, {Key: "mergeIn", Value: "feature"}}).Err()
			var conflicts bson.M
			conflictErr := mainDB.RunCommand(ctx, bson.D{{Key: "dumboConflicts", Value: 1}}).Decode(&conflicts)
			if mode == "documentTouched" || mode == "documentDivergent" {
				require.ErrorContains(t, err, "conflict")
				require.NoError(t, conflictErr)
				require.Len(t, conflicts["conflicts"], 1)
				return
			}
			require.NoError(t, err)
			require.ErrorContains(t, conflictErr, "no merge or cherry-pick in progress")
			var result bson.M
			require.NoError(t, mainDB.Collection("docs").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&result))
			require.Equal(t, bson.M{"_id": int32(1), "meta": bson.M{"owner": "bob", "region": "us"}}, result)
		})
	}
}
