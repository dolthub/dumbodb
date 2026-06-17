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
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// End-to-end tests for the dumboBranchStatus command. The commit graphs port
// dolt's BranchStatusTableFunctionScriptTests scenarios over the wire protocol.

type bsEntry struct {
	hash   string
	ahead  int64
	behind int64
}

func bsToInt64(v any) int64 {
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

// bsBranchCreate creates branch name from source on the given database.
func bsBranchCreate(t *testing.T, env *dumboDBTestEnv, dbName, source, name string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.client.Database(dbName+"@"+source).RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: name},
	}).Decode(&res), "doltBranch %q from %q", name, source)
}

// bsMerge merges source into target (target is the current branch).
func bsMerge(t *testing.T, env *dumboDBTestEnv, dbName, target, source string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.client.Database(dbName+"@"+target).RunCommand(context.Background(), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: source},
	}).Decode(&res), "doltMerge %q into %q", source, target)
}

// bsTag creates a tag pointing at the commit that hashish resolves to.
func bsTag(t *testing.T, env *dumboDBTestEnv, dbName, name, hashish string) {
	t.Helper()
	var res bson.M
	require.NoError(t, env.client.Database(dbName).RunCommand(context.Background(), bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: name},
		{Key: "hash", Value: hashish},
	}).Decode(&res), "dumboTag %q -> %q", name, hashish)
}

// bsStatus runs dumboBranchStatus against connDB and returns the decoded base
// sub-document plus a target -> entry map. targets may be a single string or a
// slice of strings.
func bsStatus(t *testing.T, env *dumboDBTestEnv, connDB, base string, targets any) (bson.M, map[string]bsEntry) {
	t.Helper()
	cmd := bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: base},
	}
	if targets != nil {
		cmd = append(cmd, bson.E{Key: "targets", Value: targets})
	}
	var res bson.M
	require.NoError(t, env.client.Database(connDB).RunCommand(context.Background(), cmd).Decode(&res),
		"dumboBranchStatus(base=%q, targets=%v)", base, targets)

	require.EqualValues(t, 1, bsToInt64(res["ok"]), "ok must be 1")
	baseDoc, _ := res["base"].(bson.M)
	require.NotNil(t, baseDoc, "base sub-document must be present")

	out := map[string]bsEntry{}
	arr, _ := res["targets"].(bson.A)
	for _, raw := range arr {
		e, ok := raw.(bson.M)
		require.True(t, ok, "each target entry must be a document, got %T", raw)
		target, _ := e["target"].(string)
		hash, _ := e["hash"].(string)
		out[target] = bsEntry{hash: hash, ahead: bsToInt64(e["commitsAhead"]), behind: bsToInt64(e["commitsBehind"])}
	}
	return baseDoc, out
}

func bsAssert(t *testing.T, m map[string]bsEntry, target string, ahead, behind int64) {
	t.Helper()
	e, ok := m[target]
	if !ok {
		t.Fatalf("no entry for target %q (got %v)", target, m)
	}
	assert.Equal(t, ahead, e.ahead, "target %q ahead", target)
	assert.Equal(t, behind, e.behind, "target %q behind", target)
	assert.Len(t, e.hash, 32, "target %q resolved hash must be 32 chars", target)
}

// bsNewDB returns a fresh database name and seeds an initial baseline commit on
// main so the database exists.
func bsNewDB(t *testing.T, env *dumboDBTestEnv) string {
	t.Helper()
	dbName := fmt.Sprintf("bstatus%d", rand.Int64N(1_000_000))
	ctx := context.Background()
	require.NoError(t, env.client.Database(dbName).Drop(ctx))
	_, err := env.client.Database(dbName).Collection("seed").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "anc", "alice")
	return dbName
}

// TestBranchStatus_DivergentGraph ports dolt's first scenario over the wire.
func TestBranchStatus_DivergentGraph(t *testing.T) {
	env := startDumboDB(t)
	dbName := bsNewDB(t, env)

	bsBranchCreate(t, env, dbName, "main", "b1") // b1 from anc
	dumboDBCommitAllowEmpty(t, env, dbName+"@main", "main")

	dumboDBCommitAllowEmpty(t, env, dbName+"@b1", "b1")
	bsBranchCreate(t, env, dbName, "b1", "b2")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b2", "b2")

	bsBranchCreate(t, env, dbName, "main", "b3")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b3", "b3")
	bsBranchCreate(t, env, dbName, "b3", "b4")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b4", "b4")
	bsBranchCreate(t, env, dbName, "b4", "b5")
	b5Hash := dumboDBCommitAllowEmpty(t, env, dbName+"@b5", "b5")

	bsTag(t, env, dbName, "t1", "b1")
	bsTag(t, env, dbName, "t5", "b5")

	connDB := dbName + "@main"

	baseDoc, m := bsStatus(t, env, connDB, "main", bson.A{"main", "b1", "b2", "b3", "b4", "b5"})
	assert.Equal(t, "main", baseDoc["target"], "base target echoes input")
	assert.Len(t, baseDoc["hash"], 32, "base hash is 32 chars")
	bsAssert(t, m, "main", 0, 0)
	bsAssert(t, m, "b1", 1, 1)
	bsAssert(t, m, "b2", 2, 1)
	bsAssert(t, m, "b3", 1, 0)
	bsAssert(t, m, "b4", 2, 0)
	bsAssert(t, m, "b5", 3, 0)

	// Tags resolve like their target branches.
	_, mt := bsStatus(t, env, connDB, "main", bson.A{"t1", "t5"})
	bsAssert(t, mt, "t1", 1, 1)
	bsAssert(t, mt, "t5", 3, 0)

	// A bare commit hash echoes back verbatim.
	_, mh := bsStatus(t, env, connDB, "main", bson.A{b5Hash})
	bsAssert(t, mh, b5Hash, 3, 0)
	assert.Equal(t, b5Hash, mh[b5Hash].hash)

	// A single target string is normalized to a one-element array.
	_, ms := bsStatus(t, env, connDB, "main", "b5")
	bsAssert(t, ms, "b5", 3, 0)
}

// TestBranchStatus_HeadResolution verifies HEAD and HEAD~N resolve against the
// connection's branch and echo back verbatim.
func TestBranchStatus_HeadResolution(t *testing.T) {
	env := startDumboDB(t)
	dbName := bsNewDB(t, env)

	bsBranchCreate(t, env, dbName, "main", "b5")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b5", "c1")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b5", "c2")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b5", "c3")

	// Connection branch is b5; HEAD/HEAD~N resolve against b5.
	_, m := bsStatus(t, env, dbName+"@b5", "main", bson.A{"HEAD", "HEAD~1", "HEAD~2"})
	bsAssert(t, m, "HEAD", 3, 0)
	bsAssert(t, m, "HEAD~1", 2, 0)
	bsAssert(t, m, "HEAD~2", 1, 0)
	// Targets echo verbatim, not the rewritten branch name.
	_, ok := m["HEAD"]
	assert.True(t, ok, "HEAD must echo verbatim")
}

// TestBranchStatus_Merge ports dolt's merge scenario over the wire.
func TestBranchStatus_Merge(t *testing.T) {
	env := startDumboDB(t)
	dbName := bsNewDB(t, env)

	bsBranchCreate(t, env, dbName, "main", "b1")
	bsBranchCreate(t, env, dbName, "main", "b2")

	dumboDBCommitAllowEmpty(t, env, dbName+"@main", "m1")
	dumboDBCommitAllowEmpty(t, env, dbName+"@main", "m2")

	dumboDBCommitAllowEmpty(t, env, dbName+"@b1", "b1c1")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b1", "b1c2")

	dumboDBCommitAllowEmpty(t, env, dbName+"@b2", "b2c1")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b2", "b2c2")
	dumboDBCommitAllowEmpty(t, env, dbName+"@b2", "b2c3")

	_, m := bsStatus(t, env, dbName+"@main", "main", bson.A{"b1", "b2"})
	bsAssert(t, m, "b1", 2, 2)
	bsAssert(t, m, "b2", 3, 2)

	_, m2 := bsStatus(t, env, dbName+"@b1", "b1", bson.A{"b2"})
	bsAssert(t, m2, "b2", 3, 2)

	bsMerge(t, env, dbName, "b2", "b1") // merge b1 into b2

	_, m3 := bsStatus(t, env, dbName+"@main", "main", bson.A{"b1", "b2"})
	bsAssert(t, m3, "b1", 2, 2)
	bsAssert(t, m3, "b2", 6, 2)

	_, m4 := bsStatus(t, env, dbName+"@b1", "b1", bson.A{"b2"})
	bsAssert(t, m4, "b2", 4, 0)
}

// TestBranchStatus_Errors verifies argument and refspec error handling.
func TestBranchStatus_Errors(t *testing.T) {
	env := startDumboDB(t)
	dbName := bsNewDB(t, env)
	ctx := context.Background()

	// Missing base.
	err := env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "targets", Value: bson.A{"main"}},
	}).Err()
	require.Error(t, err, "missing base must error")

	// Empty-string target.
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{""}},
	}).Err()
	require.Error(t, err, "empty-string target must error")
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T", err)
	assert.EqualValues(t, 2, cmdErr.Code, "empty target -> BadValue (rejected by parseRootish)")

	// Unknown target.
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{"no-such-branch"}},
	}).Err()
	require.Error(t, err, "unknown target must error")

	// Missing targets -> error (targets is required).
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
	}).Err()
	require.Error(t, err, "missing targets must error")

	// Empty targets array -> error (at least one target required).
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{}},
	}).Err()
	require.Error(t, err, "empty targets array must error")
}
