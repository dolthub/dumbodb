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

const quarantineDir = ".dumbodb_dropped_databases"

func commitDoc(t *testing.T, env *dumboDBTestEnv, dbName string, doc bson.D, msg string) {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(dbName)
	_, err := db.Collection("items").InsertOne(ctx, doc)
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "dumboCommit", Value: 1},
		{Key: "message", Value: msg},
		{Key: "author", Value: "t <t@t>"},
	}).Err())
}

func dropDB(t *testing.T, env *dumboDBTestEnv, dbName string) {
	t.Helper()
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(),
		bson.D{{Key: "dropDatabase", Value: 1}}).Err())
}

func adminRun(t *testing.T, env *dumboDBTestEnv, cmd bson.D) (bson.M, error) {
	t.Helper()
	var res bson.M
	err := env.Client.Database("admin").RunCommand(context.Background(), cmd).Decode(&res)
	return res, err
}

func TestUndrop_RestoresDataAndHistory(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	commitDoc(t, env, "shop", bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: "a"}}, "c1")
	commitDoc(t, env, "shop", bson.D{{Key: "_id", Value: 2}, {Key: "v", Value: "b"}}, "c2")

	var logBefore bson.M
	require.NoError(t, env.Client.Database("shop").RunCommand(ctx, bson.D{{Key: "dumboLog", Value: 1}}).Decode(&logBefore))
	commitsBefore := len(logBefore["commits"].(bson.A))
	require.GreaterOrEqual(t, commitsBefore, 2)

	dropDB(t, env, "shop")

	// Gone from the live data dir, present in quarantine.
	_, statErr := os.Stat(filepath.Join(env.DataDir(), "shop"))
	assert.True(t, os.IsNotExist(statErr), "live dir must be gone after drop")
	qEntries, err := os.ReadDir(filepath.Join(env.DataDir(), quarantineDir, "shop"))
	require.NoError(t, err)
	assert.Len(t, qEntries, 1, "one quarantined drop expected")

	// Undrop.
	res, err := adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "shop"}})
	require.NoError(t, err)
	assert.EqualValues(t, "shop", res["undropped"])

	// Data and history are back.
	var got bson.M
	require.NoError(t, env.Client.Database("shop").Collection("items").
		FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Decode(&got))
	assert.EqualValues(t, "b", got["v"])

	var logAfter bson.M
	require.NoError(t, env.Client.Database("shop").RunCommand(ctx, bson.D{{Key: "dumboLog", Value: 1}}).Decode(&logAfter))
	assert.Equal(t, commitsBefore, len(logAfter["commits"].(bson.A)), "history must be fully restored")
}

func TestUndrop_ListDroppable(t *testing.T) {
	env := startDumboDB(t)

	commitDoc(t, env, "alpha", bson.D{{Key: "_id", Value: 1}}, "c1")
	commitDoc(t, env, "beta", bson.D{{Key: "_id", Value: 1}}, "c1")
	dropDB(t, env, "alpha")
	dropDB(t, env, "beta")

	res, err := adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
	require.NoError(t, err)
	dropped := res["dropped"].(bson.A)
	assert.Len(t, dropped, 2)

	names := map[string]bool{}
	for _, d := range dropped {
		names[d.(bson.M)["name"].(string)] = true
	}
	assert.True(t, names["alpha"] && names["beta"])
}

func TestUndrop_KeepAllAndDisambiguate(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Drop "ledger" twice with different contents.
	commitDoc(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "first"}}, "c1")
	dropDB(t, env, "ledger")
	commitDoc(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "second"}}, "c1")
	dropDB(t, env, "ledger")

	// Both drops retained.
	qEntries, err := os.ReadDir(filepath.Join(env.DataDir(), quarantineDir, "ledger"))
	require.NoError(t, err)
	require.Len(t, qEntries, 2, "keep-all: both drops retained")

	// Undrop without dropId is ambiguous -> error.
	_, err = adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "dropid")

	// List, pick the oldest drop, restore it specifically.
	res, err := adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
	require.NoError(t, err)
	dropped := res["dropped"].(bson.A)
	require.Len(t, dropped, 2)
	// List is most-recent-first, so the last entry is the oldest ("first").
	oldest := dropped[len(dropped)-1].(bson.M)["dropId"].(string)

	_, err = adminRun(t, env, bson.D{
		{Key: "dumboUndrop", Value: 1},
		{Key: "name", Value: "ledger"},
		{Key: "dropId", Value: oldest},
	})
	require.NoError(t, err)

	var got bson.M
	require.NoError(t, env.Client.Database("ledger").Collection("items").
		FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
	assert.EqualValues(t, "first", got["gen"], "the specifically-chosen older drop should be restored")

	// One drop still remains quarantined.
	qEntries, err = os.ReadDir(filepath.Join(env.DataDir(), quarantineDir, "ledger"))
	require.NoError(t, err)
	assert.Len(t, qEntries, 1)
}

func TestUndrop_Errors(t *testing.T) {
	env := startDumboDB(t)

	// Nonexistent.
	_, err := adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ghost"}})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "no dropped database")

	// Live db already exists.
	commitDoc(t, env, "dup", bson.D{{Key: "_id", Value: 1}}, "c1")
	dropDB(t, env, "dup")
	commitDoc(t, env, "dup", bson.D{{Key: "_id", Value: 2}}, "c1") // recreate live
	_, err = adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "dup"}})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "already exists")

	// Revision-qualified name rejected.
	_, err = adminRun(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "dup@main"}})
	require.Error(t, err)
}

func TestUndrop_AdminOnly(t *testing.T) {
	env := startDumboDB(t)
	commitDoc(t, env, "secret", bson.D{{Key: "_id", Value: 1}}, "c1")
	dropDB(t, env, "secret")

	// Against a non-admin db: rejected.
	err := env.Client.Database("secret").RunCommand(context.Background(),
		bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "secret"}}).Err()
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "admin database")
}

func TestDropDatabase_SystemDatabasesProtected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	for _, sys := range []string{"admin", "config", "local"} {
		err := env.Client.Database(sys).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}}).Err()
		require.Error(t, err, "dropping system db %q must error", sys)
		assert.Contains(t, strings.ToLower(err.Error()), "system databases cannot be dropped")
	}

	// admin dir survives.
	_, statErr := os.Stat(filepath.Join(env.DataDir(), "admin"))
	assert.NoError(t, statErr, "admin dir must survive")
}

func TestDropDatabase_QuarantineNotListed(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	commitDoc(t, env, "visible", bson.D{{Key: "_id", Value: 1}}, "c1")
	dropDB(t, env, "visible")
	commitDoc(t, env, "stillhere", bson.D{{Key: "_id", Value: 1}}, "c1")

	names, err := env.Client.ListDatabaseNames(ctx, bson.D{})
	require.NoError(t, err)
	for _, n := range names {
		assert.NotContains(t, n, quarantineDir, "quarantine dir must not appear as a database")
		assert.NotEqual(t, "visible", n, "dropped db must not be listed")
	}
}
