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

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Push, pull, fetch, clone and backup reach other systems with the server's own
// filesystem access and credentials, so under --auth they are refused for
// every user. Remote configuration (add/list/remove) is a local operation
// that needs readWrite on the database.
func TestAuthorize_RemoteOperationsDisabledUnderAuth(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "reader", "read")
	createUserWithRole(t, h, "mydb", "writer", "readWrite")
	createUserWithRole(t, h, "admin", "boss", "root")

	for _, name := range []string{"dumboPush", "doltPush", "dumboFetch", "doltFetch", "dumboPull", "doltPull", "dumboClone", "doltClone", "dumboBackup", "doltBackup"} {
		msg := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(name, int32(1), "$db", "mydb"))))
		require.True(t, isUnauthorized(t, h.authorize(authCtx("boss", "admin"), msg)), "%s as root", name)
		require.True(t, isUnauthorized(t, h.authorize(authCtx("writer", "mydb"), msg)), "%s as readWrite", name)
	}

	for _, name := range []string{"dumboRemote", "doltRemote"} {
		msg := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(name, int32(1), "action", "list", "$db", "mydb"))))
		require.NoError(t, h.authorize(authCtx("writer", "mydb"), msg), "%s as readWrite", name)
		require.True(t, isUnauthorized(t, h.authorize(authCtx("reader", "mydb"), msg)), "%s as read", name)
	}
}

// Every dumboBackup action, read-only ones included, is refused under --auth
// until remote commands get access control.
func TestAuthorize_BackupDisabledUnderAuthForEveryAction(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "admin", "boss", "root")

	for _, action := range []string{"add", "list", "remove", "sync", "sync-url", "restore"} {
		msg := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("dumboBackup", int32(1), "action", action, "db", "mydb", "$db", "admin"))))
		require.True(t, isUnauthorized(t, h.authorize(authCtx("boss", "admin"), msg)), "dumboBackup %s as root", action)
	}
}
