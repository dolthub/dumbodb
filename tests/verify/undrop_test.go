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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const undropQuarantineDir = ".dumbodb_dropped_databases"

func undropCommit(t *testing.T, env *dumboDBTestEnv, dbName string, doc bson.D, msg string) {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(dbName)
	_, err := db.Collection("items").InsertOne(ctx, doc)
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1},
		{Key: "message", Value: msg},
		{Key: "author", Value: "a <a@a>"},
	}).Err())
}

func undropDropDB(t *testing.T, env *dumboDBTestEnv, dbName string) {
	t.Helper()
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(),
		bson.D{{Key: "dropDatabase", Value: 1}}).Err())
}

func undropAdmin(t *testing.T, env *dumboDBTestEnv, cmd bson.D) (bson.M, error) {
	t.Helper()
	var res bson.M
	err := env.Client.Database("admin").RunCommand(context.Background(), cmd).Decode(&res)
	return res, err
}

func undropCommitCount(t *testing.T, env *dumboDBTestEnv, dbName string) int {
	t.Helper()
	var log bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(),
		bson.D{{Key: "doltLog", Value: 1}}).Decode(&log))
	return len(log["commits"].(bson.A))
}

func droppedNames(res bson.M) []string {
	var names []string
	for _, d := range res["dropped"].(bson.A) {
		names = append(names, d.(bson.M)["name"].(string))
	}
	return names
}

// TestUndropVerify mirrors docs/verify/undrop.md.
func TestUndropVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Setup: undropvdb with two commits.
	undropCommit(t, env, "undropvdb", bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: "alpha"}}, "commit one")
	undropCommit(t, env, "undropvdb", bson.D{{Key: "_id", Value: 2}, {Key: "label", Value: "beta"}}, "commit two")
	commitCountN := undropCommitCount(t, env, "undropvdb")
	require.GreaterOrEqual(t, commitCountN, 2)

	// Scenario 1: dropDatabase soft-deletes (does not destroy).
	t.Run("Scenario1_DropSoftDeletes", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database("undropvdb").RunCommand(ctx,
			bson.D{{Key: "dropDatabase", Value: 1}}).Decode(&res))
		assert.EqualValues(t, "undropvdb", res["dropped"])

		names, err := env.Client.ListDatabaseNames(ctx, bson.D{})
		require.NoError(t, err)
		assert.NotContains(t, names, "undropvdb")

		entries, err := os.ReadDir(filepath.Join(env.DataDir(), undropQuarantineDir, "undropvdb"))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "exactly one quarantined copy")
	})

	// Scenario 2: List databases available to undrop.
	t.Run("Scenario2_ListDroppable", func(t *testing.T) {
		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		entries := res["dropped"].(bson.A)
		require.Len(t, entries, 1)
		e := entries[0].(bson.M)
		assert.EqualValues(t, "undropvdb", e["name"])
		assert.NotEmpty(t, e["dropId"])
		assert.NotNil(t, e["droppedAt"])
	})

	// Scenario 3: Undrop restores data AND full history.
	t.Run("Scenario3_UndropRestores", func(t *testing.T) {
		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "undropvdb"}})
		require.NoError(t, err)
		assert.EqualValues(t, "undropvdb", res["undropped"])

		count, err := env.Client.Database("undropvdb").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count, "both documents restored")

		assert.Equal(t, commitCountN, undropCommitCount(t, env, "undropvdb"), "full history restored")

		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		assert.NotContains(t, droppedNames(list), "undropvdb")
	})

	// Scenario 4: Repeat drops are all kept; disambiguate with dropId.
	t.Run("Scenario4_KeepAllDisambiguate", func(t *testing.T) {
		undropCommit(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "first"}}, "g1")
		undropDropDB(t, env, "ledger")
		undropCommit(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "second"}}, "g2")
		undropDropDB(t, env, "ledger")

		entries, err := os.ReadDir(filepath.Join(env.DataDir(), undropQuarantineDir, "ledger"))
		require.NoError(t, err)
		require.Len(t, entries, 2, "both drops retained")

		// Ambiguous undrop is rejected.
		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "dropid")

		// Restore the oldest copy (last in the most-recent-first list).
		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		var ledgerDrops []bson.M
		for _, d := range list["dropped"].(bson.A) {
			if dm := d.(bson.M); dm["name"] == "ledger" {
				ledgerDrops = append(ledgerDrops, dm)
			}
		}
		require.Len(t, ledgerDrops, 2)
		oldest := ledgerDrops[len(ledgerDrops)-1]["dropId"].(string)

		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}, {Key: "dropId", Value: oldest},
		})
		require.NoError(t, err)

		var got bson.M
		require.NoError(t, env.Client.Database("ledger").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "first", got["gen"], "the chosen older copy was restored")

		remaining, err := os.ReadDir(filepath.Join(env.DataDir(), undropQuarantineDir, "ledger"))
		require.NoError(t, err)
		assert.Len(t, remaining, 1, "one copy still quarantined")
	})

	// Scenario 5: Error cases.
	t.Run("Scenario5_Errors", func(t *testing.T) {
		_, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ghost"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "no dropped database")

		// ledger is live again from Scenario 4.
		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "already exists")

		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger@main"}})
		require.Error(t, err)
	})

	// Scenario 6: undrop is admin-only.
	t.Run("Scenario6_AdminOnly", func(t *testing.T) {
		err := env.Client.Database("undropvdb").RunCommand(ctx,
			bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "undropvdb"}}).Err()
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "admin database")
	})

	// Scenario 7: System databases cannot be dropped.
	t.Run("Scenario7_SystemDbsProtected", func(t *testing.T) {
		for _, sys := range []string{"admin", "config", "local"} {
			err := env.Client.Database(sys).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}}).Err()
			require.Error(t, err, "dropping %q must error", sys)
			assert.Contains(t, strings.ToLower(err.Error()), "system databases cannot be dropped")
		}
		_, statErr := os.Stat(filepath.Join(env.DataDir(), "admin"))
		assert.NoError(t, statErr, "admin dir survives")
	})
}
