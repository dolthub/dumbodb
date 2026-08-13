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

// Field-level add/modify/delete cross-product through the wire protocol, one
// cell per subtest. The document-level equivalents live in merge_matrix_test.go;
// these exercise the same matrix on a single field of a shared document, which
// takes a different path in the backend (the JSON three-way field merge rather
// than the document differ).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// fieldEdit is one branch's edit to the "status" field.
type fieldEdit struct {
	set   string
	unset bool
}

// apply reports whether it changed anything, so callers skip empty commits.
func (e fieldEdit) apply(t *testing.T, db *mongo.Database) bool {
	t.Helper()
	var update bson.D
	switch {
	case e.unset:
		update = bson.D{{Key: "$unset", Value: bson.D{{Key: "status", Value: ""}}}}
	case e.set != "":
		update = bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: e.set}}}}
	default:
		return false
	}
	_, err := db.Collection("docs").UpdateOne(context.Background(),
		bson.D{{Key: "_id", Value: int32(1)}}, update)
	require.NoError(t, err)
	return true
}

// Modify on one side against delete on the other must conflict: the two sides
// disagree about whether the value should exist, and either answer discards an
// intention. Those two cells fail today (issue 59).
var fieldMergeMatrix = []struct {
	name         string
	baseStatus   string
	ours, theirs fieldEdit
	wantConflict bool
	wantStatus   string
}{
	{
		name:       "ours inserts only",
		ours:       fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name:       "theirs inserts only",
		theirs:     fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name: "both insert same value",
		ours: fieldEdit{set: "review"}, theirs: fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name: "both insert different values",
		ours: fieldEdit{set: "review"}, theirs: fieldEdit{set: "final"},
		wantConflict: true,
	},
	{
		name:       "neither side touches",
		baseStatus: "draft",
		wantStatus: "draft",
	},
	{
		name:       "ours modifies, theirs untouched",
		baseStatus: "draft",
		ours:       fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name:       "theirs modifies, ours untouched",
		baseStatus: "draft",
		theirs:     fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name:       "both modify same value",
		baseStatus: "draft",
		ours:       fieldEdit{set: "review"}, theirs: fieldEdit{set: "review"},
		wantStatus: "review",
	},
	{
		name:       "both modify different values",
		baseStatus: "draft",
		ours:       fieldEdit{set: "review"}, theirs: fieldEdit{set: "final"},
		wantConflict: true,
	},
	{
		name:       "ours deletes, theirs untouched",
		baseStatus: "draft",
		ours:       fieldEdit{unset: true},
	},
	{
		name:       "theirs deletes, ours untouched",
		baseStatus: "draft",
		theirs:     fieldEdit{unset: true},
	},
	{
		name:       "both delete",
		baseStatus: "draft",
		ours:       fieldEdit{unset: true}, theirs: fieldEdit{unset: true},
	},
	{
		name:       "ours modifies, theirs deletes",
		baseStatus: "draft",
		ours:       fieldEdit{set: "review"}, theirs: fieldEdit{unset: true},
		wantConflict: true,
	},
	{
		name:       "theirs modifies, ours deletes",
		baseStatus: "draft",
		ours:       fieldEdit{unset: true}, theirs: fieldEdit{set: "review"},
		wantConflict: true,
	},
}

func TestMergeFieldMatrix(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	for _, tc := range fieldMergeMatrix {
		t.Run(tc.name, func(t *testing.T) {
			dbName := fmt.Sprintf("fm_%d", rand.Int64N(1_000_000))
			baseline := bson.D{{Key: "_id", Value: int32(1)}, {Key: "note", Value: "x"}}
			if tc.baseStatus != "" {
				baseline = append(baseline, bson.E{Key: "status", Value: tc.baseStatus})
			}

			db := env.Client.Database(dbName)
			require.NoError(t, db.Drop(ctx))
			_, err := db.Collection("docs").InsertOne(ctx, baseline)
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName, "baseline", "alice")

			mainDB := env.Client.Database(dbName + "@main")
			editDB := env.Client.Database(dbName + "@edit")
			var branchRaw bson.M
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)},
				{Key: "branch", Value: "edit"},
			}).Decode(&branchRaw))

			if tc.theirs.apply(t, editDB) {
				dumboDBCommit(t, env, dbName+"@edit", "edit side", "bob")
			}
			if tc.ours.apply(t, mainDB) {
				dumboDBCommit(t, env, dbName+"@main", "main side", "alice")
			}

			mergeRaw := runCommandRaw(t, mainDB, bson.D{
				{Key: "doltMerge", Value: int32(1)},
				{Key: "mergeIn", Value: "edit"},
			})

			if tc.wantConflict {
				require.EqualValues(t, 0, mergeRaw["ok"],
					"expected a conflict, merge reported: %v", mergeRaw)
				conflicts := getConflictsByCollection(t, mainDB)
				assert.Len(t, conflicts["docs"], 1, "expected one document conflict")
				return
			}

			require.EqualValues(t, 1, mergeRaw["ok"],
				"expected a clean merge, merge reported: %v", mergeRaw)

			var doc bson.M
			require.NoError(t, mainDB.Collection("docs").
				FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
			if tc.wantStatus == "" {
				assert.NotContains(t, doc, "status", "status must be absent")
			} else {
				assert.Equal(t, tc.wantStatus, doc["status"])
			}
			assert.Equal(t, "x", doc["note"], "untouched field must survive")
		})
	}
}

// The modify/delete conflict must be resolvable both ways, not a dead end.
func TestMergeFieldModifyDeleteResolution(t *testing.T) {
	cases := []struct {
		resolution string
		wantStatus string
	}{
		{resolution: "ours", wantStatus: ""},
		{resolution: "theirs", wantStatus: "review"},
	}

	env := startDumboDB(t)
	ctx := context.Background()

	for _, tc := range cases {
		t.Run(tc.resolution, func(t *testing.T) {
			dbName := fmt.Sprintf("fmr_%d", rand.Int64N(1_000_000))
			db := env.Client.Database(dbName)
			require.NoError(t, db.Drop(ctx))
			_, err := db.Collection("docs").InsertOne(ctx, bson.D{
				{Key: "_id", Value: int32(1)},
				{Key: "status", Value: "draft"},
				{Key: "note", Value: "x"},
			})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName, "baseline", "alice")

			mainDB := env.Client.Database(dbName + "@main")
			editDB := env.Client.Database(dbName + "@edit")
			var branchRaw bson.M
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)},
				{Key: "branch", Value: "edit"},
			}).Decode(&branchRaw))

			require.True(t, fieldEdit{set: "review"}.apply(t, editDB))
			dumboDBCommit(t, env, dbName+"@edit", "edit modifies status", "bob")
			require.True(t, fieldEdit{unset: true}.apply(t, mainDB))
			dumboDBCommit(t, env, dbName+"@main", "main deletes status", "alice")

			mergeRaw := runCommandRaw(t, mainDB, bson.D{
				{Key: "doltMerge", Value: int32(1)},
				{Key: "mergeIn", Value: "edit"},
			})
			require.EqualValues(t, 0, mergeRaw["ok"], "modify/delete must conflict")

			resolveAllConflicts(t, mainDB, tc.resolution)
			mergeContinue(t, mainDB)

			var doc bson.M
			require.NoError(t, mainDB.Collection("docs").
				FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
			if tc.wantStatus == "" {
				assert.NotContains(t, doc, "status")
			} else {
				assert.Equal(t, tc.wantStatus, doc["status"])
			}
			assert.Equal(t, "x", doc["note"])
		})
	}
}
