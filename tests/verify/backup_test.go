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

// TestBackupVerify is the automated analog of docs/verify/backup.md, and
// TestBackupDisabledUnderAuth of its Scenario 11.
//
// Each subtest corresponds to one scenario in that document and they run
// sequentially, sharing servers A and B and the backup directory so each
// scenario's side effects carry into the next.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const codeOperationFailed = 96

func backupCmd(fields ...bson.E) bson.D {
	return append(bson.D{{Key: "dumboBackup", Value: int32(1)}}, fields...)
}

func runBackup(t *testing.T, c *mongo.Client, fields ...bson.E) bson.M {
	t.Helper()
	var res bson.M
	require.NoError(t, c.Database("admin").RunCommand(context.Background(), backupCmd(fields...)).Decode(&res))
	require.EqualValues(t, 1, res["ok"])
	return res
}

func backupErr(c *mongo.Client, fields ...bson.E) error {
	return c.Database("admin").RunCommand(context.Background(), backupCmd(fields...)).Err()
}

func requireBackupErr(t *testing.T, err error, code int32, msg string) {
	t.Helper()
	requireCode(t, err, code)
	require.ErrorContains(t, err, msg)
}

func countIn(t *testing.T, c *mongo.Client, db, coll string) int64 {
	t.Helper()
	n, err := c.Database(db).Collection(coll).CountDocuments(context.Background(), bson.D{})
	require.NoError(t, err)
	return n
}

func databaseNames(t *testing.T, c *mongo.Client) []string {
	t.Helper()
	names, err := c.ListDatabaseNames(context.Background(), bson.D{})
	require.NoError(t, err)
	return names
}

func e(key string, value any) bson.E {
	return bson.E{Key: key, Value: value}
}

func TestBackupVerify(t *testing.T) {
	a := startDumboDB(t).Client
	b := startDumboDB(t).Client
	ctx := context.Background()
	dir := t.TempDir()
	backupURL := func(name string) string { return "file://" + filepath.Join(dir, "dumbo-backup-"+name) }
	nightly, adhoc, origin := backupURL("nightly"), backupURL("adhoc"), backupURL("origin")

	t.Run("Setup", func(t *testing.T) {
		orders := a.Database("orders")
		_, err := orders.Collection("items").InsertMany(ctx, []any{bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "_id", Value: 2}}})
		require.NoError(t, err)
		require.NoError(t, orders.RunCommand(ctx, bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "first two"}}).Err())
		require.NoError(t, orders.RunCommand(ctx, bson.D{{Key: "dumboTag", Value: 1}, {Key: "name", Value: "v1"}}).Err())
		require.NoError(t, orders.RunCommand(ctx, bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"}}).Err())

		feature := a.Database("orders@feature")
		_, err = feature.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 10}})
		require.NoError(t, err)
		require.NoError(t, feature.RunCommand(ctx, bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "feature work"}}).Err())

		_, err = orders.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 3}})
		require.NoError(t, err)
		require.NoError(t, orders.RunCommand(ctx, bson.D{{Key: "dumboRemote", Value: 1}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: origin}}).Err())

		_, err = a.Database("inventory").Collection("stock").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
		require.NoError(t, err)
	})

	t.Run("Scenario1_Add", func(t *testing.T) {
		res := runBackup(t, a, e("action", "add"), e("db", "orders"), e("name", "nightly"), e("url", nightly))
		require.Equal(t, "nightly", res["name"])
		require.Equal(t, nightly, res["url"])

		requireBackupErr(t, backupErr(a, e("action", "add"), e("db", "orders"), e("name", "nightly"), e("url", backupURL("other"))),
			codeOperationFailed, `dumboBackup add: backup "nightly" already exists for database "orders"`)
	})

	t.Run("Scenario2_List", func(t *testing.T) {
		res := runBackup(t, a, e("action", "list"), e("db", "orders"))
		backups := res["backups"].(bson.A)
		require.Len(t, backups, 1)
		require.Equal(t, bson.M{"name": "nightly", "url": nightly}, backups[0])

		res = runBackup(t, a, e("action", "list"), e("db", "inventory"))
		require.Empty(t, res["backups"])
	})

	var root1, root2 string

	t.Run("Scenario3_Sync", func(t *testing.T) {
		res := runBackup(t, a, e("action", "sync"), e("db", "orders"), e("name", "nightly"))
		require.Equal(t, nightly, res["url"])
		require.Equal(t, false, res["upToDate"])
		root1, _ = res["root"].(string)
		require.NotEmpty(t, root1)

		res = runBackup(t, a, e("action", "sync"), e("db", "orders"), e("name", "nightly"))
		require.Equal(t, true, res["upToDate"])
		require.Equal(t, root1, res["root"])
	})

	t.Run("Scenario4_RestoreOnAnotherServer", func(t *testing.T) {
		res := runBackup(t, b, e("action", "restore"), e("url", nightly), e("as", "orders"))
		require.Equal(t, "orders", res["db"])
		require.Equal(t, nightly, res["url"])
		require.Equal(t, root1, res["root"])

		require.EqualValues(t, 3, countIn(t, b, "orders", "items"), "the uncommitted insert came across")
		require.EqualValues(t, 3, countIn(t, b, "orders@feature", "items"))

		var tags bson.M
		require.NoError(t, b.Database("orders").RunCommand(ctx, bson.D{{Key: "dumboTag", Value: 1}}).Decode(&tags))
		require.Len(t, tags["tags"].(bson.A), 1)
		var status bson.M
		require.NoError(t, b.Database("orders").RunCommand(ctx, bson.D{{Key: "dumboStatus", Value: 1}}).Decode(&status))
		require.Equal(t, true, status["dirty"], "%v", status)

		var remotes bson.M
		require.NoError(t, b.Database("orders").RunCommand(ctx, bson.D{{Key: "dumboRemote", Value: 1}, {Key: "action", Value: "list"}}).Decode(&remotes))
		require.Empty(t, remotes["remotes"])
		res = runBackup(t, b, e("action", "list"), e("db", "orders"))
		require.Empty(t, res["backups"])
	})

	t.Run("Scenario5_IncrementalSync", func(t *testing.T) {
		_, err := a.Database("orders").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 4}})
		require.NoError(t, err)

		res := runBackup(t, a, e("action", "sync"), e("db", "orders"), e("name", "nightly"))
		require.Equal(t, false, res["upToDate"])
		root2, _ = res["root"].(string)
		require.NotEmpty(t, root2)
		require.NotEqual(t, root1, root2)
	})

	t.Run("Scenario6_RestoreOverExisting", func(t *testing.T) {
		_, err := b.Database("orders").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 99}})
		require.NoError(t, err)

		requireBackupErr(t, backupErr(b, e("action", "restore"), e("url", nightly), e("as", "orders")),
			codeOperationFailed, `dumboBackup restore: database "orders" already exists; use force to replace it`)
		require.EqualValues(t, 4, countIn(t, b, "orders", "items"), "a refused restore leaves the database untouched")

		res := runBackup(t, b, e("action", "restore"), e("url", nightly), e("as", "orders"), e("force", true))
		require.Equal(t, root2, res["root"])
		require.EqualValues(t, 4, countIn(t, b, "orders", "items"))
		err = b.Database("orders").Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 99}}).Err()
		require.ErrorIs(t, err, mongo.ErrNoDocuments)

		var dropped bson.M
		require.NoError(t, b.Database("admin").RunCommand(ctx, bson.D{{Key: "dumboUndrop", Value: 1}}).Decode(&dropped))
		var names []any
		for _, d := range dropped["dropped"].(bson.A) {
			names = append(names, d.(bson.M)["name"])
		}
		require.Contains(t, names, "orders")
	})

	t.Run("Scenario7_SyncURL", func(t *testing.T) {
		res := runBackup(t, a, e("action", "sync-url"), e("db", "orders"), e("url", adhoc))
		require.Equal(t, adhoc, res["url"])
		require.Equal(t, root2, res["root"])
		require.Equal(t, false, res["upToDate"])

		res = runBackup(t, a, e("action", "list"), e("db", "orders"))
		require.Len(t, res["backups"].(bson.A), 1, "sync-url registers nothing")

		runBackup(t, b, e("action", "restore"), e("url", adhoc), e("as", "orders_adhoc"))
		require.EqualValues(t, 4, countIn(t, b, "orders_adhoc", "items"))
	})

	t.Run("Scenario8_BackupURLCannotBeShared", func(t *testing.T) {
		requireBackupErr(t, backupErr(a, e("action", "add"), e("db", "orders"), e("name", "bad"), e("url", origin)),
			codeOperationFailed, `dumboBackup add: url "`+origin+`" is already used by remote "orders.origin"`)
		requireBackupErr(t, backupErr(a, e("action", "sync-url"), e("db", "inventory"), e("url", nightly)),
			codeOperationFailed, `dumboBackup sync-url: url "`+nightly+`" is already used by backup "orders.nightly"`)

		err := a.Database("inventory").RunCommand(ctx, bson.D{{Key: "dumboRemote", Value: 1}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: nightly}}).Err()
		requireBackupErr(t, err, codeOperationFailed, `dumboRemote add: url "`+nightly+`" is already used by backup "orders.nightly"`)

		err = a.Database("admin").RunCommand(ctx, bson.D{{Key: "dumboClone", Value: 1}, {Key: "from", Value: nightly}, {Key: "as", Value: "loot"}}).Err()
		requireBackupErr(t, err, codeOperationFailed, `dumboClone: url "`+nightly+`" is already used by backup "orders.nightly"`)
		require.NotContains(t, databaseNames(t, a), "loot")

		res := runBackup(t, a, e("action", "sync-url"), e("db", "orders"), e("url", nightly))
		require.Equal(t, true, res["upToDate"])
	})

	t.Run("Scenario9_RemoveKeepsData", func(t *testing.T) {
		runBackup(t, a, e("action", "remove"), e("db", "orders"), e("name", "nightly"))
		res := runBackup(t, a, e("action", "list"), e("db", "orders"))
		require.Empty(t, res["backups"])
		requireBackupErr(t, backupErr(a, e("action", "remove"), e("db", "orders"), e("name", "nightly")),
			codeOperationFailed, `dumboBackup remove: backup "nightly" not found for database "orders"`)

		runBackup(t, b, e("action", "restore"), e("url", nightly), e("as", "orders_archive"))
		require.EqualValues(t, 4, countIn(t, b, "orders_archive", "items"))
	})

	t.Run("Scenario10_Errors", func(t *testing.T) {
		requireBackupErr(t, runIn(a, "orders", backupCmd(e("action", "list"), e("db", "orders"))),
			codeOperationFailed, "dumboBackup: can only be run against the admin database")

		requireBackupErr(t, backupErr(a, e("action", "sync-url"), e("db", "admin"), e("url", backupURL("admin"))),
			codeOperationFailed, `dumboBackup: the "admin" database cannot be backed up`)
		requireBackupErr(t, backupErr(a, e("action", "restore"), e("url", adhoc), e("as", "admin"), e("force", true)),
			codeOperationFailed, `dumboBackup restore: cannot restore into the "admin" database`)

		requireBackupErr(t, backupErr(a, e("action", "sync"), e("db", "orders"), e("name", "nope")),
			codeOperationFailed, `dumboBackup sync: backup "nope" not found for database "orders"`)
		requireBackupErr(t, backupErr(a, e("action", "sync-url"), e("db", "nosuchdb"), e("url", backupURL("none"))),
			codeOperationFailed, `dumboBackup sync-url: database "nosuchdb" does not exist`)
		require.NotContains(t, databaseNames(t, a), "nosuchdb")

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "dumbo-backup-empty"), 0o755))
		requireBackupErr(t, backupErr(a, e("action", "restore"), e("url", backupURL("empty")), e("as", "x")),
			codeOperationFailed, "dumboBackup restore: no backup found at")
		require.NotContains(t, databaseNames(t, a), "x")

		requireBackupErr(t, backupErr(a, e("action", "bogus")),
			codeBadValue, "dumboBackup: action must be add, list, remove, sync, sync-url, or restore")
		requireBackupErr(t, backupErr(a, e("action", "add"), e("db", "orders"), e("name", "x")),
			codeBadValue, "dumboBackup add: url is required")
		requireBackupErr(t, backupErr(a, e("action", "restore"), e("url", adhoc)),
			codeBadValue, "dumboBackup restore: as is required")
		requireCode(t, backupErr(a, e("action", "list"), e("db", "orders"), e("bogus", 1)), codeUnknownField)
	})
}

// Scenario 11 of docs/verify/backup.md: backups use the server's own
// filesystem access and credentials, so like push, pull, fetch and clone they
// are refused for every user under --auth.
func TestBackupDisabledUnderAuth(t *testing.T) {
	env := startDumboDB(t, "--auth")
	require.NoError(t, env.Client.Database("admin").RunCommand(context.Background(), bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	root := authClient(t, env.Port, "admin", "admin-pw", "admin")
	require.NoError(t, insert(root, "orders", "items", bson.D{{Key: "_id", Value: 1}}))

	requireBackupErr(t, backupErr(root, e("action", "list"), e("db", "orders")), codeUnauthorized,
		"doltBackup is disabled when access control is enabled: push, pull, fetch, clone and backup use the server's own credentials")

	url := "file://" + filepath.Join(t.TempDir(), "bk")
	for _, fields := range [][]bson.E{
		{e("action", "add"), e("db", "orders"), e("name", "nightly"), e("url", url)},
		{e("action", "list"), e("db", "orders")},
		{e("action", "remove"), e("db", "orders"), e("name", "nightly")},
		{e("action", "sync"), e("db", "orders"), e("name", "nightly")},
		{e("action", "sync-url"), e("db", "orders"), e("url", url)},
		{e("action", "restore"), e("url", url), e("as", "copy")},
	} {
		requireCode(t, backupErr(root, fields...), codeUnauthorized)
		requireCode(t, root.Database("admin").RunCommand(context.Background(),
			append(bson.D{{Key: "doltBackup", Value: int32(1)}}, fields...)).Err(), codeUnauthorized)
	}
}
