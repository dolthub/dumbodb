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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMergeMatrix_NestedDocument(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	for _, scenario := range []struct {
		name                   string
		base                   bson.M
		oursField, theirsField string
		oursValue, theirsValue any
		want                   bson.M
		compareAndSwap         bool
		wantConflicts          [5]bool
	}{
		{"disjoint", bson.M{"_id": int32(1), "meta": bson.M{"owner": "alice", "region": "eu"}}, "meta.owner", "meta.region", "bob", "us", bson.M{"_id": int32(1), "meta": bson.M{"owner": "bob", "region": "us"}}, false, [5]bool{false, false, false, true, true}},
		{"nested CAS", bson.M{"_id": int32(1), "meta": bson.M{"version": int32(1)}}, "meta.version", "meta.version", int32(2), int32(2), bson.M{"_id": int32(1), "meta": bson.M{"version": int32(2)}}, true, [5]bool{true, true, false, true, false}},
		{"deep disjoint", bson.M{"_id": int32(1), "meta": bson.M{"inner": bson.M{"deep": bson.M{"a": "old", "b": "old"}}}}, "meta.inner.deep.a", "meta.inner.deep.b", "left", "right", bson.M{"_id": int32(1), "meta": bson.M{"inner": bson.M{"deep": bson.M{"a": "left", "b": "right"}}}}, false, [5]bool{false, false, false, true, true}},
		{"deep divergent", bson.M{"_id": int32(1), "meta": bson.M{"inner": bson.M{"deep": bson.M{"value": "old"}}}}, "meta.inner.deep.value", "meta.inner.deep.value", "left", "right", nil, false, [5]bool{true, true, true, true, true}},
	} {
		// Expectation order: default, fieldTouched, fieldDivergent, documentTouched, documentDivergent.
		for modeIndex, mode := range []string{"", "fieldTouched", "fieldDivergent", "documentTouched", "documentDivergent"} {
			name := mode
			if name == "" {
				name = "default"
			}
			t.Run(scenario.name+"/"+name, func(t *testing.T) {
				dbName := fmt.Sprintf("nested_%d", rand.Int64N(1_000_000))
				mainDB := env.Client.Database(dbName + "@main")
				create := bson.D{{Key: "create", Value: "docs"}}
				if mode != "" {
					create = append(create, bson.E{Key: "mergeMode", Value: mode})
				}
				require.NoError(t, mainDB.RunCommand(ctx, create).Err())
				_, err := mainDB.Collection("docs").InsertOne(ctx, scenario.base)
				require.NoError(t, err)
				dumboDBCommit(t, env, dbName+"@main", "seed")
				require.NoError(t, mainDB.RunCommand(ctx, bson.D{
					{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
				}).Err())
				featureDB := env.Client.Database(dbName + "@feature")
				updateBranch := func(database *mongo.Database, field string, value any) {
					filter := bson.D{{Key: "_id", Value: int32(1)}}
					update := bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: value}}}}
					if scenario.compareAndSwap {
						filter = append(filter, bson.E{Key: "meta.version", Value: int32(1)})
						update = bson.D{{Key: "$inc", Value: bson.D{{Key: "meta.version", Value: int32(1)}}}}
					}
					result, updateErr := database.Collection("docs").UpdateOne(ctx, filter, update)
					require.NoError(t, updateErr)
					require.EqualValues(t, 1, result.MatchedCount, "each branch must match its own seeded version")
				}
				updateBranch(featureDB, scenario.theirsField, scenario.theirsValue)
				dumboDBCommit(t, env, dbName+"@feature", "feature edits "+scenario.theirsField)
				updateBranch(mainDB, scenario.oursField, scenario.oursValue)
				dumboDBCommit(t, env, dbName+"@main", "main edits "+scenario.oursField)
				err = mainDB.RunCommand(ctx, bson.D{{Key: "dumboMerge", Value: 1}, {Key: "mergeIn", Value: "feature"}}).Err()
				var conflicts bson.M
				conflictErr := mainDB.RunCommand(ctx, bson.D{{Key: "dumboConflicts", Value: 1}}).Decode(&conflicts)
				if scenario.wantConflicts[modeIndex] {
					require.ErrorContains(t, err, "conflict")
					require.NoError(t, conflictErr)
					require.Len(t, conflicts["conflicts"], 1)
					entries, ok := conflicts["conflicts"].(bson.A)
					require.True(t, ok)
					envelope, ok := entries[0].(bson.M)
					require.True(t, ok)
					require.Equal(t, "docs", envelope["collection"])
					require.Equal(t, "document", envelope["type"])
					expectedSide := func(field string, value any) bson.M {
						var copyDocument func(bson.M) bson.M
						copyDocument = func(source bson.M) bson.M {
							copied := make(bson.M, len(source))
							for key, value := range source {
								if nested, ok := value.(bson.M); ok {
									value = copyDocument(nested)
								}
								copied[key] = value
							}
							return copied
						}
						document := copyDocument(scenario.base)
						components := strings.Split(field, ".")
						parent := document
						for _, component := range components[:len(components)-1] {
							parent = parent[component].(bson.M)
						}
						parent[components[len(components)-1]] = value
						return document
					}
					for _, side := range []struct {
						name     string
						document bson.M
					}{
						{"base", scenario.base},
						{"ours", expectedSide(scenario.oursField, scenario.oursValue)},
						{"theirs", expectedSide(scenario.theirsField, scenario.theirsValue)},
					} {
						content, ok := envelope[side.name].(bson.M)
						require.True(t, ok, side.name)
						require.Equal(t, int32(1), content["_id"], side.name)
						require.Equal(t, side.document, content["doc"], side.name)
					}
					return
				}
				require.NoError(t, err)
				require.ErrorContains(t, conflictErr, "no merge or cherry-pick in progress")
				var result bson.M
				require.NoError(t, mainDB.Collection("docs").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&result))
				require.Equal(t, scenario.want, result)
			})
		}
	}
}
