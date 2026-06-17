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

// TestBranchStatusVerify is the automated analog of docs/verify/branch-status.md.
//
// Each top-level subtest corresponds to one scenario in that document. The setup
// builds the divergent graph from the doc's Setup block. dumboBranchStatus is
// read-only, so the subtests run sequentially against the single shared database
// without affecting one another.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBranchStatusVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("bsvrfy%d", rand.Int64N(1_000_000))

	require.NoError(t, env.client.Database(dbName).Drop(ctx))
	_, err := env.client.Database(dbName).Collection("seed").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "anc", "alice <alice@acme.com>")

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
	b5Head := dumboDBCommitAllowEmpty(t, env, dbName+"@b5", "b5")

	bsTag(t, env, dbName, "t1", "b1")
	bsTag(t, env, dbName, "t5", "b5")

	connMain := dbName + "@main"

	t.Run("Scenario1_AcrossBranches", func(t *testing.T) {
		baseDoc, m := bsStatus(t, env, connMain, "main", bson.A{"main", "b1", "b2", "b3", "b4", "b5"})
		assert.Equal(t, "main", baseDoc["target"])
		assert.Len(t, baseDoc["hash"], 32)
		bsAssert(t, m, "main", 0, 0)
		bsAssert(t, m, "b1", 1, 1)
		bsAssert(t, m, "b2", 2, 1)
		bsAssert(t, m, "b3", 1, 0)
		bsAssert(t, m, "b4", 2, 0)
		bsAssert(t, m, "b5", 3, 0)
	})

	t.Run("Scenario2_Tags", func(t *testing.T) {
		_, m := bsStatus(t, env, connMain, "main", bson.A{"t1", "t5"})
		bsAssert(t, m, "t1", 1, 1)
		bsAssert(t, m, "t5", 3, 0)
	})

	t.Run("Scenario3_HeadResolution", func(t *testing.T) {
		_, m := bsStatus(t, env, dbName+"@b5", "main", bson.A{"HEAD", "HEAD~1", "HEAD~2"})
		bsAssert(t, m, "HEAD", 3, 0)
		bsAssert(t, m, "HEAD~1", 2, 0)
		bsAssert(t, m, "HEAD~2", 1, 0)
	})

	t.Run("Scenario4_SingleStringAndHash", func(t *testing.T) {
		_, ms := bsStatus(t, env, connMain, "main", "b5")
		bsAssert(t, ms, "b5", 3, 0)

		_, mh := bsStatus(t, env, connMain, "main", bson.A{b5Head})
		bsAssert(t, mh, b5Head, 3, 0)
		assert.Equal(t, b5Head, mh[b5Head].hash, "bare hash echoes and resolves to itself")
	})

	// rel = merge of b2 (anc->b1->b2) into main (anc->main): 3 ahead (b1, b2, merge).
	t.Run("Scenario5_MergeCommit", func(t *testing.T) {
		bsBranchCreate(t, env, dbName, "main", "rel")
		bsMerge(t, env, dbName, "rel", "b2")

		_, m := bsStatus(t, env, connMain, "main", bson.A{"rel"})
		bsAssert(t, m, "rel", 3, 0)
	})

	t.Run("Scenario6_Errors", func(t *testing.T) {
		// missing targets
		require.Error(t, env.client.Database(connMain).RunCommand(ctx, bson.D{
			{Key: "dumboBranchStatus", Value: int32(1)},
			{Key: "base", Value: "main"},
		}).Err())

		// empty targets array
		require.Error(t, env.client.Database(connMain).RunCommand(ctx, bson.D{
			{Key: "dumboBranchStatus", Value: int32(1)},
			{Key: "base", Value: "main"},
			{Key: "targets", Value: bson.A{}},
		}).Err())

		// empty-string target
		require.Error(t, env.client.Database(connMain).RunCommand(ctx, bson.D{
			{Key: "dumboBranchStatus", Value: int32(1)},
			{Key: "base", Value: "main"},
			{Key: "targets", Value: bson.A{""}},
		}).Err())

		// unknown target
		require.Error(t, env.client.Database(connMain).RunCommand(ctx, bson.D{
			{Key: "dumboBranchStatus", Value: int32(1)},
			{Key: "base", Value: "main"},
			{Key: "targets", Value: bson.A{"no-such-branch"}},
		}).Err())

		// missing base
		require.Error(t, env.client.Database(connMain).RunCommand(ctx, bson.D{
			{Key: "dumboBranchStatus", Value: int32(1)},
			{Key: "targets", Value: bson.A{"b1"}},
		}).Err())
	})
}
