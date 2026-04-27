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
)

// TestAutoCommit verifies that --auto-commit causes each document write
// (insert, update, delete) to produce its own Dolt commit automatically,
// without any explicit doltCommit call.
func TestAutoCommit(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()

	dbName := fmt.Sprintf("autocommit%d", rand.Int64N(1_000_000))

	// Ensure a clean slate.
	require.NoError(t, env.client.Database(dbName).Drop(ctx))

	coll := env.client.Database(dbName).Collection("items")

	// Insert a document — should auto-commit.
	_, err := coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "val", Value: "alpha"},
	})
	require.NoError(t, err)

	t.Run("AfterInsert", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
		}).Decode(&raw))
		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 2, "expected Initialize + 1 auto-insert commit")
		assert.Equal(t, "auto: insert into items", lr.Commits[0].Message)
		assert.Equal(t, "Initialize database", lr.Commits[1].Message)
	})

	// Update the document — should auto-commit.
	_, err = coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "val", Value: "beta"}}}},
	)
	require.NoError(t, err)

	t.Run("AfterUpdate", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
		}).Decode(&raw))
		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 3, "expected Initialize + insert + update commits")
		assert.Equal(t, "auto: update items", lr.Commits[0].Message)
		assert.Equal(t, "auto: insert into items", lr.Commits[1].Message)
		assert.Equal(t, "Initialize database", lr.Commits[2].Message)
	})

	// Delete the document — should auto-commit.
	_, err = coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)

	t.Run("AfterDelete", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
		}).Decode(&raw))
		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 4, "expected Initialize + insert + update + delete commits")
		assert.Equal(t, "auto: delete from items", lr.Commits[0].Message)
		assert.Equal(t, "auto: update items", lr.Commits[1].Message)
		assert.Equal(t, "auto: insert into items", lr.Commits[2].Message)
		assert.Equal(t, "Initialize database", lr.Commits[3].Message)
	})
}
