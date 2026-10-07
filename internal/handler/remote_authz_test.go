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

// Remote operations open URLs with the server's own filesystem access and
// credentials, so before the fix any authenticated user could use them to
// read host files, reach internal services, or spend the operator's cloud
// and DoltHub credentials. They now need a cluster-level privilege.
func TestAuthorize_RemoteCommandsRequireClusterPrivilege(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "owner", "dbOwner")
	createUserWithRole(t, h, "admin", "anyRW", "readWriteAnyDatabase")
	createUserWithRole(t, h, "admin", "boss", "root")

	for _, name := range []string{
		"dumboRemote", "doltRemote", "dumboPush", "doltPush", "dumboFetch", "doltFetch",
		"dumboPull", "doltPull", "dumboClone", "doltClone",
	} {
		msg := must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(name, int32(1), "$db", "mydb"))))
		require.True(t, isUnauthorized(t, h.authorize(authCtx("owner", "mydb"), msg)), "%s as dbOwner", name)
		require.True(t, isUnauthorized(t, h.authorize(authCtx("anyRW", "admin"), msg)), "%s as readWriteAnyDatabase", name)
		require.NoError(t, h.authorize(authCtx("boss", "admin"), msg), "%s as root", name)
	}
}
