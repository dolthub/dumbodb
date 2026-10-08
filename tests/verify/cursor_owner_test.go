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

const codeCursorNotFound = 43

func openCursor(t *testing.T, c *mongo.Client, db, coll string) int64 {
	t.Helper()
	var res bson.M
	require.NoError(t, c.Database(db).RunCommand(context.Background(), bson.D{
		{Key: "find", Value: coll}, {Key: "batchSize", Value: 1},
	}).Decode(&res))
	id, ok := res["cursor"].(bson.M)["id"].(int64)
	require.True(t, ok, "%v", res)
	require.NotZero(t, id)
	return id
}

// Before the fix cursor ownership compared the bare username, so a user named
// "admin" in another database could page through or kill the cursors of
// admin@admin. Cursor IDs were also sequential, so they were easy to guess.
func TestCursorOwnershipIsPerPrincipal(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"},
		{Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	adminRun(t, admin, "tenant", bson.D{{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}}})
	for i := range 5 {
		require.NoError(t, insert(admin, "appdb", "secrets", bson.D{{Key: "_id", Value: i}}))
	}

	victimCursor := openCursor(t, admin, "appdb", "secrets")

	impostor := authClient(t, port, "admin", "pw", "tenant")
	err := impostor.Database("appdb").RunCommand(ctx, bson.D{
		{Key: "getMore", Value: victimCursor}, {Key: "collection", Value: "secrets"},
	}).Err()
	requireCode(t, err, codeCursorNotFound)

	var killed bson.M
	require.NoError(t, impostor.Database("appdb").RunCommand(ctx, bson.D{
		{Key: "killCursors", Value: "secrets"}, {Key: "cursors", Value: bson.A{victimCursor}},
	}).Decode(&killed))
	require.Empty(t, killed["cursorsKilled"])

	require.NoError(t, admin.Database("appdb").RunCommand(ctx, bson.D{
		{Key: "getMore", Value: victimCursor}, {Key: "collection", Value: "secrets"},
	}).Err(), "the owner can still use its cursor")

	next := openCursor(t, admin, "appdb", "secrets")
	diff := next - victimCursor
	require.False(t, diff >= -2 && diff <= 2, "cursor ids must not be sequential: %d then %d", victimCursor, next)
}

// Before the fix the owner was user + "@" + db unescaped, so alice@tenant in
// auth database 123 and alice in auth database tenant@123 owned each
// other's cursors.
func TestCursorOwnershipWithDelimiterInIdentity(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	readAppdb := bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}
	adminRun(t, admin, "123", bson.D{{Key: "createUser", Value: "alice@tenant"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: readAppdb}})
	adminRun(t, admin, "tenant@123", bson.D{{Key: "createUser", Value: "alice"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: readAppdb}})
	for i := range 5 {
		require.NoError(t, insert(admin, "appdb", "secrets", bson.D{{Key: "_id", Value: i}}))
	}

	owner := authClient(t, port, "alice@tenant", "pw", "123")
	other := authClient(t, port, "alice", "pw", "tenant@123")
	id := openCursor(t, owner, "appdb", "secrets")

	err := other.Database("appdb").RunCommand(ctx, bson.D{{Key: "getMore", Value: id}, {Key: "collection", Value: "secrets"}}).Err()
	requireCode(t, err, codeCursorNotFound)

	var killed bson.M
	require.NoError(t, other.Database("appdb").RunCommand(ctx, bson.D{
		{Key: "killCursors", Value: "secrets"}, {Key: "cursors", Value: bson.A{id}},
	}).Decode(&killed))
	require.Empty(t, killed["cursorsKilled"])

	require.NoError(t, owner.Database("appdb").RunCommand(ctx, bson.D{{Key: "getMore", Value: id}, {Key: "collection", Value: "secrets"}}).Err())
}
