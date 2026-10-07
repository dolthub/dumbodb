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

// Before the fix the lowercase aliases findandmodify and dbstats were
// registered but the privilege table only listed findAndModify and dbStats,
// and authorization looked up the literal command name, so the aliases ran
// with no privilege check at all.
func TestCommandAliasesAreAuthorized(t *testing.T) {
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
	require.NoError(t, insert(admin, "victim", "secrets", bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: "secret"}}))
	require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: 1}}))

	reader := authClient(t, port, "reader", "pw", "appdb")

	findAndModify := func(name, db, coll string) error {
		return reader.Database(db).RunCommand(ctx, bson.D{
			{Key: name, Value: coll},
			{Key: "query", Value: bson.D{{Key: "_id", Value: 1}}},
			{Key: "update", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "pwned"}}}}},
		}).Err()
	}

	for _, name := range []string{"findAndModify", "findandmodify"} {
		requireCode(t, findAndModify(name, "victim", "secrets"), codeUnauthorized)
		requireCode(t, findAndModify(name, "appdb", "things"), codeUnauthorized)
		requireCode(t, findAndModify(name, "admin@main", "system.users"), codeUnauthorized)
	}

	var doc bson.M
	require.NoError(t, admin.Database("victim").Collection("secrets").FindOne(ctx, bson.D{}).Decode(&doc))
	require.Equal(t, "secret", doc["v"])

	for _, name := range []string{"dbStats", "dbstats"} {
		requireCode(t, reader.Database("victim").RunCommand(ctx, bson.D{{Key: name, Value: 1}}).Err(), codeUnauthorized)
		require.NoError(t, reader.Database("appdb").RunCommand(ctx, bson.D{{Key: name, Value: 1}}).Err())
	}
}
