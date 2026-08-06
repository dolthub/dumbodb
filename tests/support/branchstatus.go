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
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type BsEntry struct {
	Hash   string
	Ahead  int64
	Behind int64
}

func BsToInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

func BsBranchCreate(t *testing.T, env *Env, dbName, source, name string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.Client.Database(dbName+"@"+source).RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: name},
	}).Decode(&res), "doltBranch %q from %q", name, source)
}

// BsMerge merges source into target (target is the current branch).
func BsMerge(t *testing.T, env *Env, dbName, target, source string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.Client.Database(dbName+"@"+target).RunCommand(context.Background(), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: source},
	}).Decode(&res), "doltMerge %q into %q", source, target)
}

// BsTag creates a tag pointing at the commit that hashish resolves to.
func BsTag(t *testing.T, env *Env, dbName, name, hashish string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(), bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: name},
		{Key: "hash", Value: hashish},
	}).Decode(&res), "dumboTag %q -> %q", name, hashish)
}

// BsStatus runs dumboBranchStatus against connDB and returns the decoded base
// sub-document plus a target -> entry map. targets may be a single string or a
// slice of strings.
func BsStatus(t *testing.T, env *Env, connDB, base string, targets any) (bson.M, map[string]BsEntry) {
	t.Helper()
	cmd := bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: base},
	}
	if targets != nil {
		cmd = append(cmd, bson.E{Key: "targets", Value: targets})
	}
	var res bson.M
	require.NoError(t, env.Client.Database(connDB).RunCommand(context.Background(), cmd).Decode(&res),
		"dumboBranchStatus(base=%q, targets=%v)", base, targets)

	require.EqualValues(t, 1, BsToInt64(res["ok"]), "ok must be 1")
	baseDoc, _ := res["base"].(bson.M)
	require.NotNil(t, baseDoc, "base sub-document must be present")

	out := map[string]BsEntry{}
	arr, _ := res["targets"].(bson.A)
	for _, raw := range arr {
		e, ok := raw.(bson.M)
		require.True(t, ok, "each target entry must be a document, got %T", raw)
		target, _ := e["target"].(string)
		hash, _ := e["hash"].(string)
		out[target] = BsEntry{Hash: hash, Ahead: BsToInt64(e["commitsAhead"]), Behind: BsToInt64(e["commitsBehind"])}
	}
	return baseDoc, out
}

func BsAssert(t *testing.T, m map[string]BsEntry, target string, ahead, behind int64) {
	t.Helper()
	e, ok := m[target]
	if !ok {
		t.Fatalf("no entry for target %q (got %v)", target, m)
	}
	assert.Equal(t, ahead, e.Ahead, "target %q ahead", target)
	assert.Equal(t, behind, e.Behind, "target %q behind", target)
	assert.Len(t, e.Hash, 32, "target %q resolved hash must be 32 chars", target)
}

// BsNewDB returns a fresh database name and seeds an initial baseline commit on
// main so the database exists.
func BsNewDB(t *testing.T, env *Env) string {
	t.Helper()
	dbName := fmt.Sprintf("bstatus%d", rand.Int64N(1_000_000))
	ctx := context.Background()
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	_, err := env.Client.Database(dbName).Collection("seed").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	Commit(t, env, dbName, "anc", "alice")
	return dbName
}
