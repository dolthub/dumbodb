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

// commitIdentityDoc is the {name,email} shape stored per user and echoed by usersInfo.
type commitIdentityDoc struct {
	Name  string `bson:"name"`
	Email string `bson:"email"`
}

func TestCommitIdentityUsersInfo(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	// Bootstrap the first admin via the localhost exception.
	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"},
		{Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	admin := authClient(t, port, "admin", "admin-pw", "admin")

	readIdentity := func(db, user string) *commitIdentityDoc {
		var res struct {
			Users []struct {
				User           string             `bson:"user"`
				CommitIdentity *commitIdentityDoc `bson:"commitIdentity"`
			} `bson:"users"`
		}
		require.NoError(t, admin.Database(db).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: user}}).Decode(&res))
		require.Len(t, res.Users, 1)
		return res.Users[0].CommitIdentity
	}

	rwRole := bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "appid"}}}

	t.Run("full identity round-trips", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "full"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Alice Example"}, {Key: "email", Value: "alice@acme.com"}}},
		})
		id := readIdentity("appid", "full")
		require.NotNil(t, id)
		require.Equal(t, "Alice Example", id.Name)
		require.Equal(t, "alice@acme.com", id.Email)
	})

	t.Run("name-only identity", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "nameonly"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bob"}}},
		})
		id := readIdentity("appid", "nameonly")
		require.NotNil(t, id)
		require.Equal(t, "Bob", id.Name)
		require.Empty(t, id.Email)
	})

	t.Run("no identity", func(t *testing.T) {
		adminRun(t, admin, "appid", bson.D{
			{Key: "createUser", Value: "plain"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
		})
		require.Nil(t, readIdentity("appid", "plain"))
	})

	t.Run("invalid email rejected", func(t *testing.T) {
		err := admin.Database("appid").RunCommand(ctx, bson.D{
			{Key: "createUser", Value: "bad"}, {Key: "pwd", Value: "pw"}, {Key: "roles", Value: rwRole},
			{Key: "commitIdentity", Value: bson.D{{Key: "name", Value: "Bad"}, {Key: "email", Value: "not-an-email"}}},
		}).Err()
		requireCode(t, err, codeBadValue)
	})

	t.Run("commitIdentity survives showCustomData:false", func(t *testing.T) {
		var res struct {
			Users []struct {
				CommitIdentity *commitIdentityDoc `bson:"commitIdentity"`
			} `bson:"users"`
		}
		require.NoError(t, admin.Database("appid").RunCommand(ctx, bson.D{
			{Key: "usersInfo", Value: "full"}, {Key: "showCustomData", Value: false},
		}).Decode(&res))
		require.Len(t, res.Users, 1)
		require.NotNil(t, res.Users[0].CommitIdentity)
		require.Equal(t, "alice@acme.com", res.Users[0].CommitIdentity.Email)
	})
}
