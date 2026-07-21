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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	codeUnauthorized      = 13
	codeRoleNotFound      = 31
	codeBadValue          = 2
	codeRoleAlreadyExists = 51002
)

func authURI(port int, user, pwd, authSource string) string {
	return fmt.Sprintf("mongodb://%s:%s@127.0.0.1:%d/?authSource=%s", user, pwd, port, authSource)
}

// dialAs connects as the given user, forcing the SCRAM handshake and returning
// any authentication error. A nil error means the credentials were accepted.
func dialAs(t *testing.T, port int, user, pwd, authSource string) (*mongo.Client, error) {
	t.Helper()
	c, err := mongo.Connect(options.Client().ApplyURI(authURI(port, user, pwd, authSource)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	err = c.Database(authSource).RunCommand(context.Background(), bson.D{{Key: "connectionStatus", Value: 1}}).Err()
	return c, err
}

// authClient connects as a user and requires authentication to succeed.
func authClient(t *testing.T, port int, user, pwd, authSource string) *mongo.Client {
	t.Helper()
	c, err := dialAs(t, port, user, pwd, authSource)
	require.NoError(t, err, "auth as %s should succeed", user)
	return c
}

func errCode(err error) int32 {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code
	}
	var we mongo.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 {
		return int32(we.WriteErrors[0].Code)
	}
	return -1
}

func requireCode(t *testing.T, err error, code int32) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errCode(err), "unexpected error: %v", err)
}

func find(c *mongo.Client, db, coll string) error {
	return c.Database(db).RunCommand(context.Background(),
		bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: bson.D{}}}).Err()
}

func insert(c *mongo.Client, db, coll string, doc bson.D) error {
	return c.Database(db).RunCommand(context.Background(),
		bson.D{{Key: "insert", Value: coll}, {Key: "documents", Value: bson.A{doc}}}).Err()
}

func adminRun(t *testing.T, c *mongo.Client, db string, cmd bson.D) {
	t.Helper()
	require.NoError(t, c.Database(db).RunCommand(context.Background(), cmd).Err())
}

func TestAuthRBACVerify(t *testing.T) {
	env := startDumboDB(t, "--auth")
	ctx := context.Background()
	port := env.Port

	// Scenario 1: the auth gate rejects real work while unauthenticated, and the
	// localhost exception permits creating the first user.
	requireCode(t, find(env.Client, "appdb", "things"), codeUnauthorized)
	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "admin"},
		{Key: "pwd", Value: "admin-pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}},
	}).Err())
	requireCode(t, find(env.Client, "appdb", "things"), codeUnauthorized)

	admin := authClient(t, port, "admin", "admin-pw", "admin")

	// Persistent app-database users reused across scenarios.
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "reader"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}}})
	adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "writer"}, {Key: "pwd", Value: "pw"},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "appdb"}}}}})
	require.NoError(t, insert(admin, "appdb", "things", bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: "seed"}}))

	reader := authClient(t, port, "reader", "pw", "appdb")
	writer := authClient(t, port, "writer", "pw", "appdb")

	t.Run("Scenario01_GateAndBootstrap", func(t *testing.T) {
		// Re-assert the gate now that a user exists: still unauthenticated here.
		requireCode(t, find(env.Client, "appdb", "things"), codeUnauthorized)
	})

	t.Run("Scenario02_SCRAM", func(t *testing.T) {
		var res bson.M
		require.NoError(t, admin.Database("admin").RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&res))
		users, _ := res["authInfo"].(bson.M)["authenticatedUsers"].(bson.A)
		require.Len(t, users, 1)
		require.Equal(t, "admin", users[0].(bson.M)["user"])

		_, err := dialAs(t, port, "admin", "wrong", "admin")
		require.Error(t, err)
		require.Contains(t, strings.ToLower(err.Error()), "auth")
	})

	t.Run("Scenario03_BuiltinRoles", func(t *testing.T) {
		require.NoError(t, find(reader, "appdb", "things"))
		requireCode(t, insert(reader, "appdb", "things", bson.D{{Key: "_id", Value: 2}}), codeUnauthorized)
		require.NoError(t, insert(writer, "appdb", "things", bson.D{{Key: "_id", Value: 2}}))
	})

	t.Run("Scenario04_CustomRole", func(t *testing.T) {
		adminRun(t, admin, "appdb", bson.D{{Key: "createRole", Value: "inserter"},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"insert"}}}}},
			{Key: "roles", Value: bson.A{}}})
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "ins"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "inserter"}, {Key: "db", Value: "appdb"}}}}})

		ins := authClient(t, port, "ins", "pw", "appdb")
		require.NoError(t, insert(ins, "appdb", "things", bson.D{{Key: "_id", Value: 10}}))
		requireCode(t, find(ins, "appdb", "things"), codeUnauthorized)
	})

	t.Run("Scenario05_GrantRevokePrivilege", func(t *testing.T) {
		ins := authClient(t, port, "ins", "pw", "appdb")
		requireCode(t, find(ins, "appdb", "things"), codeUnauthorized)

		adminRun(t, admin, "appdb", bson.D{{Key: "grantPrivilegesToRole", Value: "inserter"},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"find"}}}}}})
		require.NoError(t, find(ins, "appdb", "things"), "grant applies to the live connection")

		adminRun(t, admin, "appdb", bson.D{{Key: "revokePrivilegesFromRole", Value: "inserter"},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"find"}}}}}})
		requireCode(t, find(ins, "appdb", "things"), codeUnauthorized)
	})

	t.Run("Scenario06_RoleInheritance", func(t *testing.T) {
		adminRun(t, admin, "appdb", bson.D{{Key: "createRole", Value: "finder"},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"find"}}}}},
			{Key: "roles", Value: bson.A{}}})
		adminRun(t, admin, "appdb", bson.D{{Key: "createRole", Value: "wrapper"},
			{Key: "privileges", Value: bson.A{}},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "finder"}, {Key: "db", Value: "appdb"}}}}})
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "wrap"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "wrapper"}, {Key: "db", Value: "appdb"}}}}})

		wrap := authClient(t, port, "wrap", "pw", "appdb")
		require.NoError(t, find(wrap, "appdb", "things"))

		var res bson.M
		require.NoError(t, admin.Database("appdb").RunCommand(ctx, bson.D{
			{Key: "rolesInfo", Value: "wrapper"}, {Key: "showPrivileges", Value: true}}).Decode(&res))
		roles := res["roles"].(bson.A)
		inh := roles[0].(bson.M)["inheritedPrivileges"].(bson.A)
		require.NotEmpty(t, inh, "wrapper inherits finder's privileges")
	})

	t.Run("Scenario07_DynamicUserRoles", func(t *testing.T) {
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "dyn"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{}}})
		dyn := authClient(t, port, "dyn", "pw", "appdb")
		requireCode(t, find(dyn, "appdb", "things"), codeUnauthorized)

		adminRun(t, admin, "appdb", bson.D{{Key: "grantRolesToUser", Value: "dyn"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}}})
		require.NoError(t, find(dyn, "appdb", "things"), "grant applies to the live connection")

		adminRun(t, admin, "appdb", bson.D{{Key: "revokeRolesFromUser", Value: "dyn"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}}})
		requireCode(t, find(dyn, "appdb", "things"), codeUnauthorized)
	})

	t.Run("Scenario08_SelfService", func(t *testing.T) {
		require.NoError(t, reader.Database("appdb").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: "reader"}}).Err())
		requireCode(t, reader.Database("appdb").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: "writer"}}).Err(), codeUnauthorized)

		adminRun(t, admin, "appdb", bson.D{{Key: "createRole", Value: "selfmgr"},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"changeOwnPassword"}}}}},
			{Key: "roles", Value: bson.A{}}})
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "self"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "selfmgr"}, {Key: "db", Value: "appdb"}}}}})

		self := authClient(t, port, "self", "pw", "appdb")
		require.NoError(t, self.Database("appdb").RunCommand(ctx, bson.D{{Key: "updateUser", Value: "self"}, {Key: "pwd", Value: "newpw"}}).Err())
		requireCode(t, self.Database("appdb").RunCommand(ctx, bson.D{{Key: "updateUser", Value: "reader"}, {Key: "pwd", Value: "x"}}).Err(), codeUnauthorized)
	})

	t.Run("Scenario09_AuthenticationRestrictions", func(t *testing.T) {
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "restricted"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}},
			{Key: "authenticationRestrictions", Value: bson.A{bson.D{{Key: "clientSource", Value: bson.A{"10.0.0.1"}}}}}})
		_, err := dialAs(t, port, "restricted", "pw", "appdb")
		require.Error(t, err, "auth from a non-permitted client source must fail")
		require.Contains(t, strings.ToLower(err.Error()), "auth")

		adminRun(t, admin, "appdb", bson.D{{Key: "dropUser", Value: "restricted"}})
		adminRun(t, admin, "appdb", bson.D{{Key: "createUser", Value: "restricted"}, {Key: "pwd", Value: "pw"},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}},
			{Key: "authenticationRestrictions", Value: bson.A{bson.D{{Key: "clientSource", Value: bson.A{"127.0.0.1"}}}}}})
		_, err = dialAs(t, port, "restricted", "pw", "appdb")
		require.NoError(t, err, "auth from a permitted client source must succeed")
	})

	t.Run("Scenario10_AdminReserved", func(t *testing.T) {
		requireCode(t, insert(admin, "admin", "probe", bson.D{{Key: "_id", Value: 1}}), codeUnauthorized)
	})

	t.Run("Scenario11_ConnectionStatusPrivileges", func(t *testing.T) {
		var res bson.M
		require.NoError(t, reader.Database("appdb").RunCommand(ctx, bson.D{
			{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}).Decode(&res))
		ai := res["authInfo"].(bson.M)
		roles := ai["authenticatedUserRoles"].(bson.A)
		require.Equal(t, "read", roles[0].(bson.M)["role"])
		require.NotEmpty(t, ai["authenticatedUserPrivileges"].(bson.A))
	})

	t.Run("Scenario12_SingleAuthPerConnection", func(t *testing.T) {
		c, err := mongo.Connect(options.Client().ApplyURI(authURI(port, "admin", "admin-pw", "admin")).
			SetMaxPoolSize(1).SetMinPoolSize(1).SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
		require.NoError(t, c.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err())

		var start bson.M
		firstMsg := []byte("n,,n=admin,r=verifynonce123456789")
		require.NoError(t, c.Database("admin").RunCommand(ctx, bson.D{
			{Key: "saslStart", Value: 1},
			{Key: "mechanism", Value: "SCRAM-SHA-256"},
			{Key: "payload", Value: bson.Binary{Subtype: 0x00, Data: firstMsg}},
		}).Decode(&start))

		err = c.Database("admin").RunCommand(ctx, bson.D{
			{Key: "saslContinue", Value: 1},
			{Key: "conversationId", Value: start["conversationId"]},
			{Key: "payload", Value: bson.Binary{Subtype: 0x00, Data: []byte("c=biws,r=x,p=x")}},
		}).Err()
		require.Error(t, err, "a second authentication on a connection must be rejected")
		require.Contains(t, strings.ToLower(err.Error()), "authentication failed")
	})

	t.Run("Scenario13_RoleManagementErrors", func(t *testing.T) {
		adminRun(t, admin, "appdb", bson.D{{Key: "createRole", Value: "dup"}, {Key: "privileges", Value: bson.A{}}, {Key: "roles", Value: bson.A{}}})
		requireCode(t, admin.Database("appdb").RunCommand(ctx, bson.D{
			{Key: "createRole", Value: "dup"}, {Key: "privileges", Value: bson.A{}}, {Key: "roles", Value: bson.A{}}}).Err(), codeRoleAlreadyExists)

		requireCode(t, admin.Database("appdb").RunCommand(ctx, bson.D{
			{Key: "createRole", Value: "orphan"}, {Key: "privileges", Value: bson.A{}},
			{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "ghost"}, {Key: "db", Value: "appdb"}}}}}).Err(), codeRoleNotFound)

		requireCode(t, admin.Database("appdb").RunCommand(ctx, bson.D{{Key: "dropRole", Value: "read"}}).Err(), codeBadValue)
	})

}
