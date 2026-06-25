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

package support

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RunLog issues a doltLog command with the given extra fields and returns the
// decoded raw response.
func RunLog(t *testing.T, env *Env, dbName string, extra bson.D) bson.M {
	t.Helper()
	cmd := append(bson.D{{Key: "doltLog", Value: int32(1)}}, extra...)
	var raw bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(), cmd).Decode(&raw))
	return raw
}

// LogCommitIDs extracts the ordered commit ids from a doltLog response.
func LogCommitIDs(t *testing.T, raw bson.M) []string {
	t.Helper()
	arr, ok := raw["commits"].(bson.A)
	require.True(t, ok, "commits should be an array")
	ids := make([]string, len(arr))
	for i, c := range arr {
		ids[i] = c.(bson.M)["commitId"].(string)
	}
	return ids
}

// LogNext extracts the "next" frontier as a string slice (nil when absent).
func LogNext(t *testing.T, raw bson.M) []string {
	t.Helper()
	v, ok := raw["next"]
	if !ok {
		return nil
	}
	arr, ok := v.(bson.A)
	require.True(t, ok, "next should be an array")
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		require.True(t, ok, "next elements should be strings")
		out[i] = s
	}
	return out
}
