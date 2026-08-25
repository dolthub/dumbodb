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

func TestInsertOnNonexistentBranchIsRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("missingbranch%d", rand.Int64N(1_000_000))
	mainDB := env.Client.Database(dbName)
	require.NoError(t, mainDB.Drop(ctx))
	_, err := mainDB.Collection("nodes").InsertOne(ctx, bson.D{{Key: "_id", Value: "base"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "base", "tester <tester@example.com>")

	missingBranchDB := env.Client.Database(dbName + "@neverMade")
	_, err = missingBranchDB.Collection("nodes").InsertOne(ctx, bson.D{{Key: "_id", Value: "ghost"}})
	require.Error(t, err, "insert through a nonexistent branch must fail")

	commandError, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 96, commandError.Code)
	assert.Contains(t, commandError.Message, "rootish \"neverMade\": not found")
}
