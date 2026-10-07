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

// Before the fix the admin database's history could be rewritten like any
// other database's: resetting admin to an earlier commit restored dropped
// users, revoked roles and old passwords, and fetching foreign commits into
// admin and resetting to them planted arbitrary users. Reading admin's
// history also exposed every user's SCRAM credentials to roles that may not
// read admin.system.users.
func TestAdminHistoryIsProtected(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"}, {Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")
	adminRun(t, admin, "admin", bson.D{{Key: "createUser", Value: "anyrw"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{"readWriteAnyDatabase"}}})
	adminRun(t, admin, "admin", bson.D{{Key: "createUser", Value: "anyread"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{"readAnyDatabase"}}})
	adminRun(t, admin, "admin", bson.D{{Key: "createUser", Value: "fired"}, {Key: "pwd", Value: "old-pw"},
		{Key: "roles", Value: bson.A{"root"}}})

	var log bson.M
	require.NoError(t, admin.Database("admin").RunCommand(ctx, bson.D{{Key: "dumboLog", Value: 1}}).Decode(&log),
		"root may read admin's history")
	commits, _ := log["commits"].(bson.A)
	require.NotEmpty(t, commits, "%v", log)
	adminRun(t, admin, "admin", bson.D{{Key: "dropUser", Value: "fired"}})
	require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: 1}}))

	anyrw := authClient(t, port, "anyrw", "pw", "admin")
	anyread := authClient(t, port, "anyread", "pw", "admin")

	t.Run("HistoryCannotBeRewritten", func(t *testing.T) {
		for _, c := range []struct {
			name string
			cmd  bson.D
		}{
			{"reset", bson.D{{Key: "dumboReset", Value: 1}, {Key: "to", Value: "main~1"}, {Key: "hard", Value: true}}},
			{"commit", bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "x"}}},
			{"revert", bson.D{{Key: "dumboRevert", Value: 1}, {Key: "commit", Value: "main"}}},
			{"branch", bson.D{{Key: "dumboBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "b"}}},
			{"fetch", bson.D{{Key: "dumboFetch", Value: 1}, {Key: "from", Value: "origin"}}},
			{"pull", bson.D{{Key: "dumboPull", Value: 1}, {Key: "from", Value: "origin"}}},
		} {
			requireCode(t, runIn(admin, "admin", c.cmd), codeUnauthorized)
			requireCode(t, runIn(anyrw, "admin", c.cmd), codeUnauthorized)
		}

		_, err := dialAs(t, port, "fired", "old-pw", "admin")
		require.Error(t, err, "a dropped user must stay dropped")
	})

	t.Run("HistoryIsReadableOnlyWithCredentialAccess", func(t *testing.T) {
		requireCode(t, runIn(anyread, "admin", bson.D{{Key: "dumboDiff", Value: 1}, {Key: "from", Value: "main~1"}}), codeUnauthorized)
		requireCode(t, runIn(anyread, "admin", bson.D{{Key: "dumboLog", Value: 1}}), codeUnauthorized)
		requireCode(t, runIn(anyrw, "admin", bson.D{{Key: "dumboLog", Value: 1}}), codeUnauthorized)
		require.NoError(t, runIn(anyread, "appdb", bson.D{{Key: "dumboLog", Value: 1}}), "other databases are unaffected")
	})
}

// The reservation holds without access control too, as for direct writes.
func TestAdminHistoryIsReservedWithoutAuth(t *testing.T) {
	env := startDumboDB(t)
	err := runIn(env.Client, "admin", bson.D{{Key: "dumboReset", Value: 1}, {Key: "to", Value: "main"}, {Key: "hard", Value: true}})
	requireCode(t, err, codeUnauthorized)
	require.NoError(t, runIn(env.Client, "admin", bson.D{{Key: "dumboLog", Value: 1}}))
}
