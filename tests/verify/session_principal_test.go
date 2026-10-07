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
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func listLocalSessions(t *testing.T, c *mongo.Client, spec bson.D) []bson.M {
	t.Helper()
	cursor, err := c.Database("admin").Aggregate(context.Background(), bson.A{bson.D{{Key: "$listLocalSessions", Value: spec}}})
	require.NoError(t, err)
	var out []bson.M
	require.NoError(t, cursor.All(context.Background(), &out))
	return out
}

func sessionUID(doc bson.M) []byte {
	id, _ := doc["_id"].(bson.M)
	uid, _ := id["uid"].(bson.Binary)
	return uid.Data
}

// Session ownership uses an unambiguous principal, but the uid reported by
// $listLocalSessions stays SHA256("user@db") as in MongoDB, and filtering by
// one of two identities whose "user@db" strings collide must not return the
// other's sessions.
func TestSessionPrincipalWithDelimiterInIdentity(t *testing.T) {
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
	require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: 1}}))

	owner := authClient(t, port, "alice@tenant", "pw", "123")
	session, err := owner.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	require.NoError(t, mongo.WithSession(ctx, session, func(sc context.Context) error {
		return owner.Database("appdb").Collection("things").FindOne(sc, bson.D{}).Err()
	}))

	mine := listLocalSessions(t, owner, bson.D{})
	require.NotEmpty(t, mine)
	want := sha256.Sum256([]byte("alice@tenant@123"))
	for _, s := range mine {
		require.Equal(t, want[:], sessionUID(s))
	}

	other := listLocalSessions(t, admin, bson.D{{Key: "users", Value: bson.A{bson.D{{Key: "user", Value: "alice"}, {Key: "db", Value: "tenant@123"}}}}})
	require.Empty(t, other, "sessions of alice@tenant/123 must not be listed for alice/tenant@123")

	listed := listLocalSessions(t, admin, bson.D{{Key: "users", Value: bson.A{bson.D{{Key: "user", Value: "alice@tenant"}, {Key: "db", Value: "123"}}}}})
	require.NotEmpty(t, listed)
}
