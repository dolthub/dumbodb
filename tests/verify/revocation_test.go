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
)

// Before the fix getMore needed only a login and cursor ownership, so a user
// whose read access was revoked, or who was dropped, kept paging through any
// cursor opened beforehand. getMore now needs find on the cursor's
// collection, re-checked at each call.
func TestRevokedUserCannotContinueCursor(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	readAppdb := bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "revoked"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: readAppdb}})
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "dropped"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: readAppdb}})
	for i := range 10 {
		require.NoError(t, insert(admin, "appdb", "secrets", bson.D{{Key: "_id", Value: i}}))
	}

	revoked := authClient(t, port, "revoked", "pw", "appdb")
	dropped := authClient(t, port, "dropped", "pw", "appdb")
	revokedCursor := openCursor(t, revoked, "appdb", "secrets")
	droppedCursor := openCursor(t, dropped, "appdb", "secrets")

	adminRun(t, admin, "appdb", bson.D{{Key: "revokeRolesFromUser", Value: "revoked"}, {Key: "roles", Value: readAppdb}})
	adminRun(t, admin, "appdb", bson.D{{Key: "dropUser", Value: "dropped"}})

	getMore := bson.D{{Key: "getMore", Value: revokedCursor}, {Key: "collection", Value: "secrets"}}
	requireCode(t, runIn(revoked, "appdb", getMore), codeUnauthorized)
	getMore = bson.D{{Key: "getMore", Value: droppedCursor}, {Key: "collection", Value: "secrets"}}
	requireCode(t, runIn(dropped, "appdb", getMore), codeUnauthorized)

	requireCode(t, find(revoked, "appdb", "secrets"), codeUnauthorized)
	requireCode(t, find(dropped, "appdb", "secrets"), codeUnauthorized)
}

// A user who keeps find can still page through their cursor, including
// database-level aggregate cursors that need no collection privilege.
func TestCursorsStillWorkWithPrivileges(t *testing.T) {
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
	for i := range 10 {
		require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: i}}))
	}
	reader := authClient(t, port, "reader", "pw", "appdb")

	id := openCursor(t, reader, "appdb", "things")
	require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "getMore", Value: id}, {Key: "collection", Value: "things"}}))

	var res bson.M
	docs := bson.A{}
	for i := range 5 {
		docs = append(docs, bson.D{{Key: "n", Value: i}})
	}
	require.NoError(t, reader.Database("appdb").RunCommand(ctx, bson.D{
		{Key: "aggregate", Value: 1},
		{Key: "pipeline", Value: bson.A{bson.D{{Key: "$documents", Value: docs}}}},
		{Key: "cursor", Value: bson.D{{Key: "batchSize", Value: 1}}},
	}).Decode(&res))
	aggID := res["cursor"].(bson.M)["id"].(int64)
	require.NotZero(t, aggID)
	require.NoError(t, runIn(reader, "appdb", bson.D{{Key: "getMore", Value: aggID}, {Key: "collection", Value: "$cmd.aggregate"}}))
}
