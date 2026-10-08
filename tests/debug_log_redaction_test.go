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

package tests

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Before the fix --log-level debug dumped every request and response body,
// including createUser passwords, and the same records were served by
// getLog, so anyone with getLog (or the log file) could read them.
func TestDebugLogRedactsSecrets(t *testing.T) {
	env := startDumboDB(t, "--log-level", "debug")
	ctx := context.Background()
	admin := env.Client.Database("admin")

	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "loguser"}, {Key: "pwd", Value: "Sup3rS3cretPw"}, {Key: "roles", Value: bson.A{}},
	}).Err())
	require.NoError(t, env.Client.Database("redact").RunCommand(ctx, bson.D{
		{Key: "dumboRemote", Value: 1}, {Key: "action", Value: "add"}, {Key: "name", Value: "r"},
		{Key: "url", Value: "https://someone:t0kenValue@example.com/org/repo"},
	}).Err())

	var res bson.M
	require.NoError(t, admin.RunCommand(ctx, bson.D{{Key: "getLog", Value: "global"}}).Decode(&res))
	log := fmt.Sprint(res["log"])
	require.Contains(t, log, "createUser", "debug request logging should still be present")
	require.NotContains(t, log, "Sup3rS3cretPw")
	require.NotContains(t, log, "t0kenValue")
}
