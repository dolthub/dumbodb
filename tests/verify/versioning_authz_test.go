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
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func runIn(c *mongo.Client, db string, cmd bson.D) error {
	return c.Database(db).RunCommand(context.Background(), cmd).Err()
}

// Before the fix no version-control command was in the privilege table, and a
// command missing from the table was allowed for any authenticated user, so a
// user with no roles could read any database's history (credential hashes in
// admin included), reset or delete branches anywhere, and purge dropped
// databases.
func TestVersioningCommandsAreAuthorized(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "reader"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}}})
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "writer"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "appdb"}}}}})
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "nobody"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{}}})
	require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: 1}}))
	require.NoError(t, insert(admin, "victim", "secrets", bson.D{{Key: "_id", Value: 1}}))
	adminRun(t, admin, "victim", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "seed"}})

	reader := authClient(t, port, "reader", "pw", "appdb")
	writer := authClient(t, port, "writer", "pw", "appdb")
	nobody := authClient(t, port, "nobody", "pw", "appdb")

	t.Run("NoRolesGetsNothing", func(t *testing.T) {
		for _, db := range []string{"admin", "victim", "appdb"} {
			requireCode(t, runIn(nobody, db, bson.D{{Key: "dumboLog", Value: 1}}), codeUnauthorized)
			requireCode(t, runIn(nobody, db, bson.D{{Key: "doltDiff", Value: 1}}), codeUnauthorized)
		}
		requireCode(t, runIn(nobody, "admin", bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "purgeMatching", Value: bson.D{{Key: "name", Value: "x"}}}}), codeUnauthorized)
	})

	t.Run("ReadRoleCanReadHistoryOfItsDatabaseOnly", func(t *testing.T) {
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "dumboLog", Value: 1}}))
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "dumboStatus", Value: 1}}))
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "list"}}))
		requireCode(t, runIn(reader, "victim", bson.D{{Key: "dumboLog", Value: 1}}), codeUnauthorized)
		requireCode(t, runIn(reader, "admin", bson.D{{Key: "dumboDiff", Value: 1}}), codeUnauthorized)
		requireCode(t, runIn(reader, "appdb", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "x"}}), codeUnauthorized)
		requireCode(t, runIn(reader, "appdb", bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "f"}}), codeUnauthorized)
		requireCode(t, runIn(reader, "appdb", bson.D{{Key: "dumboReset", Value: 1}, {Key: "hard", Value: true}}), codeUnauthorized)
	})

	t.Run("WriteRoleCanChangeHistoryOfItsDatabaseOnly", func(t *testing.T) {
		require.NoError(t, runIn(writer, "appdb", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "w"}}))
		require.NoError(t, runIn(writer, "appdb", bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"}}))
		require.NoError(t, runIn(writer, "appdb@feature", bson.D{{Key: "dumboLog", Value: 1}}))
		requireCode(t, runIn(writer, "victim", bson.D{{Key: "dumboReset", Value: 1}, {Key: "hard", Value: true}}), codeUnauthorized)
		requireCode(t, runIn(writer, "victim", bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "x"}}), codeUnauthorized)
		requireCode(t, runIn(writer, "appdb", bson.D{{Key: "dumboGC", Value: 1}}), codeUnauthorized)
		requireCode(t, runIn(writer, "admin", bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "purgeMatching", Value: bson.D{{Key: "name", Value: "x"}}}}), codeUnauthorized)
	})

	t.Run("RootKeepsAccess", func(t *testing.T) {
		require.NoError(t, runIn(admin, "victim", bson.D{{Key: "dumboLog", Value: 1}}))
		require.NoError(t, runIn(admin, "appdb", bson.D{{Key: "dumboGC", Value: 1}}))
		require.NoError(t, runIn(admin, "admin", bson.D{{Key: "dumboUndrop", Value: 1}}))
	})

	t.Run("UnlistedCommandsAreDenied", func(t *testing.T) {
		requireCode(t, runIn(admin, "admin", bson.D{{Key: "debugError", Value: "x"}}), codeUnauthorized)
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "listCommands", Value: 1}}))
	})

	t.Run("CursorsStillWork", func(t *testing.T) {
		for i := 2; i < 6; i++ {
			require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: i}}))
		}
		id := openCursor(t, reader, "appdb", "things")
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "getMore", Value: id}, {Key: "collection", Value: "things"}}))
		require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "killCursors", Value: "things"}, {Key: "cursors", Value: bson.A{id}}}))
	})
}
