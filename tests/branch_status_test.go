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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// End-to-end tests for the dumboBranchStatus command. The commit graphs port
// dolt's BranchStatusTableFunctionScriptTests scenarios over the wire protocol.


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

	_, mt := bsStatus(t, env, connDB, "main", bson.A{"t1", "t5"})
	bsAssert(t, mt, "t1", 1, 1)
	bsAssert(t, mt, "t5", 3, 0)

	_, mh := bsStatus(t, env, connDB, "main", bson.A{b5Hash})
	bsAssert(t, mh, b5Hash, 3, 0)
	assert.Equal(t, b5Hash, mh[b5Hash].Hash)

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

	_, m := bsStatus(t, env, dbName+"@b5", "main", bson.A{"HEAD", "HEAD~1", "HEAD~2"})
	bsAssert(t, m, "HEAD", 3, 0)
	bsAssert(t, m, "HEAD~1", 2, 0)
	bsAssert(t, m, "HEAD~2", 1, 0)
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

	err := env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "targets", Value: bson.A{"main"}},
	}).Err()
	require.Error(t, err, "missing base must error")

	err = env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{""}},
	}).Err()
	require.Error(t, err, "empty-string target must error")
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T", err)
	assert.EqualValues(t, 2, cmdErr.Code, "empty target -> BadValue (rejected by parseRootish)")

	err = env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{"no-such-branch"}},
	}).Err()
	require.Error(t, err, "unknown target must error")

	err = env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
	}).Err()
	require.Error(t, err, "missing targets must error")

	err = env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboBranchStatus", Value: int32(1)},
		{Key: "base", Value: "main"},
		{Key: "targets", Value: bson.A{}},
	}).Err()
	require.Error(t, err, "empty targets array must error")
}
