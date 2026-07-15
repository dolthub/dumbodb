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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func acMessages(t *testing.T, db *mongo.Database) []string {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "doltLog", Value: int32(1)}}).Decode(&raw))
	lr := decodeLogResult(t, raw)
	msgs := make([]string, len(lr.Commits))
	for i, c := range lr.Commits {
		msgs[i] = c.Message
	}
	return msgs
}

// acMatrixDB returns a fresh database seeded with one doc, so the database-init
// commit already exists and subsequent single-command writes are clean +1
// deltas (the first write to a brand-new database otherwise also produces the
// Initialize commit).
func acMatrixDB(t *testing.T, env *dumboDBTestEnv) *mongo.Database {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("acm_%d", rand.Int64N(1_000_000)))
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("_seed").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(0)}})
	require.NoError(t, err)
	return db
}

// TestAutoCommit_Insert covers insert shapes: single, multi-doc (one command),
// and into an existing collection -- each exactly one commit with a counted
// message.
func TestAutoCommit_Insert(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := acMatrixDB(t, env)
	coll := db.Collection("items")

	before := acCommitCount(t, db)
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	assert.Equal(t, before+1, acCommitCount(t, db), "single insert = 1 commit")

	_, err = coll.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: int32(2)}}, bson.D{{Key: "_id", Value: int32(3)}}, bson.D{{Key: "_id", Value: int32(4)}},
	})
	require.NoError(t, err)
	assert.Equal(t, before+2, acCommitCount(t, db), "multi-doc insert = 1 commit")
	assert.Equal(t, "auto: insert 3 docs into items", acMessages(t, db)[0])
}

// TestAutoCommit_UpdateDelete covers update/delete shapes and the no-op guard.
func TestAutoCommit_UpdateDelete(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := acMatrixDB(t, env)
	coll := db.Collection("items")
	_, err := coll.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: 0}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: 0}},
	})
	require.NoError(t, err)

	n := acCommitCount(t, db)
	_, err = coll.UpdateMany(ctx, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}})
	require.NoError(t, err)
	assert.Equal(t, n+1, acCommitCount(t, db), "multi update = 1 commit")
	assert.Equal(t, "auto: update items", acMessages(t, db)[0])

	n = acCommitCount(t, db)
	_, err = coll.DeleteMany(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	assert.Equal(t, n+1, acCommitCount(t, db), "delete = 1 commit")
	assert.Equal(t, "auto: delete from items", acMessages(t, db)[0])

	// No-op update / delete -> zero commits.
	n = acCommitCount(t, db)
	_, err = coll.UpdateMany(ctx, bson.D{{Key: "_id", Value: int32(999)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 9}}}})
	require.NoError(t, err)
	_, err = coll.DeleteMany(ctx, bson.D{{Key: "_id", Value: int32(999)}})
	require.NoError(t, err)
	assert.Equal(t, n, acCommitCount(t, db), "no-op update/delete = 0 commits")
}

// TestAutoCommit_FindAndModify covers findAndModify update/remove/upsert.
func TestAutoCommit_FindAndModify(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := acMatrixDB(t, env)
	coll := db.Collection("items")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: 0}})
	require.NoError(t, err)

	n := acCommitCount(t, db)
	require.NoError(t, coll.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}}).Err())
	assert.Equal(t, n+1, acCommitCount(t, db), "findAndModify update = 1 commit")

	n = acCommitCount(t, db)
	require.NoError(t, coll.FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Err())
	assert.Equal(t, n+1, acCommitCount(t, db), "findAndModify remove = 1 commit")
}

// TestAutoCommit_Aggregate covers $out and $merge, one commit each.
func TestAutoCommit_Aggregate(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := acMatrixDB(t, env)
	src := db.Collection("src")
	_, err := src.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: 1}}, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: 2}},
	})
	require.NoError(t, err)

	n := acCommitCount(t, db)
	cur, err := src.Aggregate(ctx, mongo.Pipeline{{{Key: "$out", Value: "out_coll"}}})
	require.NoError(t, err)
	require.NoError(t, cur.Close(ctx))
	assert.Equal(t, n+1, acCommitCount(t, db), "$out = 1 commit")

	n = acCommitCount(t, db)
	cur, err = src.Aggregate(ctx, mongo.Pipeline{{{Key: "$merge", Value: bson.D{{Key: "into", Value: "merge_coll"}}}}})
	require.NoError(t, err)
	require.NoError(t, cur.Close(ctx))
	assert.Equal(t, n+1, acCommitCount(t, db), "$merge = 1 commit")
}

// TestAutoCommit_DDL covers create/drop/rename collection and index DDL, each
// one commit with a specific message, plus the reported-bug regression.
func TestAutoCommit_DDL(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	// Unseeded db so the log is exactly the operations below plus Initialize.
	db := env.Client.Database(fmt.Sprintf("acmddl_%d", rand.Int64N(1_000_000)))
	require.NoError(t, db.Drop(ctx))

	// Reported bug: create + drop + import are three distinct commits.
	require.NoError(t, db.CreateCollection(ctx, "delete_me"))
	require.NoError(t, db.Collection("delete_me").Drop(ctx))
	_, err := db.Collection("orders").InsertMany(ctx, []any{bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "_id", Value: int32(2)}}})
	require.NoError(t, err)
	require.Equal(t, []string{
		"auto: insert 2 docs into orders",
		"auto: drop collection delete_me",
		"auto: create collection delete_me",
		"Initialize database",
	}, acMessages(t, db))

	// Index create/drop with name-bearing messages.
	name, err := db.Collection("orders").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "cat", Value: int32(1)}}})
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("auto: create index %s on orders", name), acMessages(t, db)[0])
	require.NoError(t, db.Collection("orders").Indexes().DropOne(ctx, name))
	assert.Equal(t, fmt.Sprintf("auto: drop index %s on orders", name), acMessages(t, db)[0])

	// Rename.
	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: db.Name() + ".orders"},
		{Key: "to", Value: db.Name() + ".purchases"},
	}).Err())
	assert.Equal(t, "auto: rename collection orders to purchases", acMessages(t, db)[0])
}

// TestAutoCommit_BulkWriteSummary verifies one commit for the whole bulkWrite
// with a summary message.
func TestAutoCommit_BulkWriteSummary(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := acMatrixDB(t, env)
	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(100)}, {Key: "v", Value: 0}})
	require.NoError(t, err)

	before := acCommitCount(t, db)
	cmd := bson.D{
		{Key: "bulkWrite", Value: int32(1)},
		{Key: "ops", Value: bson.A{
			bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: int32(1)}}}},
			bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: int32(2)}}}},
			bson.D{{Key: "update", Value: int32(0)}, {Key: "filter", Value: bson.D{{Key: "_id", Value: int32(100)}}}, {Key: "updateMods", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}}}},
			bson.D{{Key: "delete", Value: int32(0)}, {Key: "filter", Value: bson.D{{Key: "_id", Value: int32(1)}}}},
		}},
		{Key: "nsInfo", Value: bson.A{bson.D{{Key: "ns", Value: db.Name() + ".items"}}}},
	}
	require.InDelta(t, 1.0, runCommandRaw(t, env.Client.Database("admin"), cmd)["ok"], 0.0001)

	assert.Equal(t, before+1, acCommitCount(t, db), "one bulkWrite = one commit")
	assert.Equal(t, "auto: bulkWrite (2 inserted, 1 updated, 1 deleted)", acMessages(t, db)[0])
}

// TestAutoCommit_BranchScoping verifies a write on a non-main branch commits to
// that branch, leaving main untouched.
func TestAutoCommit_BranchScoping(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbBase := fmt.Sprintf("acbr_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbBase + "@main")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())

	mainBefore := acCommitCount(t, main)
	feat := env.Client.Database(dbBase + "@feature")
	_, err = feat.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)

	assert.Equal(t, mainBefore+1, acCommitCount(t, feat), "write on feature commits to feature")
	assert.Equal(t, mainBefore, acCommitCount(t, main), "main must be untouched by a feature write")
}
