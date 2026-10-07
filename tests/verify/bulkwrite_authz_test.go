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

func bulkWrite(c *mongo.Client, ns string, ops bson.A) (bson.M, error) {
	var res bson.M
	err := c.Database("admin").RunCommand(context.Background(), bson.D{
		{Key: "bulkWrite", Value: 1},
		{Key: "ops", Value: ops},
		{Key: "nsInfo", Value: bson.A{bson.D{{Key: "ns", Value: ns}}}},
	}).Decode(&res)
	return res, err
}

func requireBulkWriteRejected(t *testing.T, res bson.M, err error) {
	t.Helper()
	if err != nil {
		return
	}
	require.NotZero(t, res["nErrors"], "bulkWrite op must be rejected: %v", res)
}

// Before the fix bulkWrite had no authorization and was not covered by the
// admin write guard, so any authenticated user could write
// admin.system.users and grant themselves root.
func TestBulkWriteAuthorization(t *testing.T) {
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
	require.NoError(t, insert(admin, "victim", "secrets", bson.D{{Key: "_id", Value: 1}}))

	reader := authClient(t, port, "reader", "pw", "appdb")
	writer := authClient(t, port, "writer", "pw", "appdb")

	t.Run("CannotGrantSelfRoot", func(t *testing.T) {
		res, err := bulkWrite(reader, "admin.system.users", bson.A{bson.D{
			{Key: "update", Value: 0},
			{Key: "filter", Value: bson.D{{Key: "user", Value: "reader"}, {Key: "db", Value: "appdb"}}},
			{Key: "updateMods", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "roles", Value: bson.A{
				bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}},
			}}}}}},
		}})
		requireBulkWriteRejected(t, res, err)

		again := authClient(t, port, "reader", "pw", "appdb")
		requireCode(t, insert(again, "appdb", "things", bson.D{{Key: "_id", Value: 99}}), codeUnauthorized)
	})

	t.Run("RootCannotWriteReservedAdmin", func(t *testing.T) {
		res, err := bulkWrite(admin, "admin.system.users", bson.A{bson.D{
			{Key: "insert", Value: 0}, {Key: "document", Value: bson.D{{Key: "_id", Value: "admin.evil"}}},
		}})
		requireBulkWriteRejected(t, res, err)
	})

	t.Run("RequiresWritePrivilegeOnEachNamespace", func(t *testing.T) {
		insertOp := bson.A{bson.D{{Key: "insert", Value: 0}, {Key: "document", Value: bson.D{{Key: "_id", Value: 7}}}}}

		res, err := bulkWrite(reader, "appdb.things", insertOp)
		requireCode(t, err, codeUnauthorized)
		_ = res

		_, err = bulkWrite(writer, "victim.secrets", bson.A{bson.D{{Key: "delete", Value: 0}, {Key: "filter", Value: bson.D{}}, {Key: "multi", Value: true}}})
		requireCode(t, err, codeUnauthorized)

		res, err = bulkWrite(writer, "appdb.things", insertOp)
		require.NoError(t, err)
		require.EqualValues(t, 1, res["nInserted"])
	})
}
