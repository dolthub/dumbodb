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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func seedCommittedDB(t *testing.T, env *dumboDBTestEnv, name string) {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(name)
	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "dumboCommit", Value: 1},
		{Key: "message", Value: "seed"},
		{Key: "author", Value: "t <t@t>"},
	}).Err())
}

func TestDropDatabase_RootNameDropsEntireDatabase(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	seedCommittedDB(t, env, "rootdrop")

	dbDir := filepath.Join(env.DataDir(), "rootdrop")
	_, statErr := os.Stat(dbDir)
	require.NoError(t, statErr, "db dir must exist before drop")

	var res bson.M
	require.NoError(t, env.Client.Database("rootdrop").RunCommand(ctx,
		bson.D{{Key: "dropDatabase", Value: 1}}).Decode(&res))
	assert.EqualValues(t, "rootdrop", res["dropped"])

	_, statErr = os.Stat(dbDir)
	assert.True(t, os.IsNotExist(statErr), "db dir must be removed after drop, got %v", statErr)
}

func TestDropDatabase_BranchQualifiedNameRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	seedCommittedDB(t, env, "branchdrop")

	dbDir := filepath.Join(env.DataDir(), "branchdrop")

	err := env.Client.Database("branchdrop@main").RunCommand(ctx,
		bson.D{{Key: "dropDatabase", Value: 1}}).Err()
	require.Error(t, err, "dropDatabase on a branch-qualified name must error")
	assert.Contains(t, strings.ToLower(err.Error()), "root database")

	_, statErr := os.Stat(dbDir)
	assert.NoError(t, statErr, "db dir must survive a rejected drop")
}

func TestDropDatabase_RevisionQualifiedNameRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	seedCommittedDB(t, env, "revdrop")

	dbDir := filepath.Join(env.DataDir(), "revdrop")

	err := env.Client.Database("revdrop@main~1").RunCommand(ctx,
		bson.D{{Key: "dropDatabase", Value: 1}}).Err()
	require.Error(t, err, "dropDatabase on a revision-qualified name must error")
	assert.Contains(t, strings.ToLower(err.Error()), "root database")

	_, statErr := os.Stat(dbDir)
	assert.NoError(t, statErr, "db dir must survive a rejected drop")
}
