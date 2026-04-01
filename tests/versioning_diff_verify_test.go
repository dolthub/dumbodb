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

// TestDiffVerify is the automated analog of docs/verify/diff.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Baseline commit (hashBase): items = [ {_id:1, label:"alpha", score:10},
//     {_id:2, label:"beta", score:20} ]
//   - Working-set changes (not committed after setup):
//     _id:3 added, _id:1 score modified (10→99), _id:2 deleted
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// diffResult holds the decoded top-level response from a dongoDiff command.
type diffResult struct {
	Collections []collDiffResult
}

// collDiffResult holds the decoded per-collection section of a dongoDiff response.
type collDiffResult struct {
	Name     string
	Added    []bson.M
	Removed  []bson.M
	Modified []modifiedDocResult
}

// modifiedDocResult holds one entry from the "modified" list in a dongoDiff response.
type modifiedDocResult struct {
	ID   any
	Diff []fieldDiffResult
}

// fieldDiffResult holds one entry from the "diff" list of a modified document.
type fieldDiffResult struct {
	Type string
	Path string
	A    any
	B    any
}

// decodeDiffResult parses the raw bson.M from a dongoDiff RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func decodeDiffResult(t *testing.T, raw bson.M) diffResult {
	t.Helper()

	rawColls, ok := raw["collections"]
	require.True(t, ok, "dongoDiff result missing 'collections' field")

	collsArr, ok := rawColls.(bson.A)
	require.True(t, ok, "dongoDiff 'collections' is not an array, got %T", rawColls)

	var out diffResult

	for _, c := range collsArr {
		cm, ok := c.(bson.M)
		require.True(t, ok, "collections entry is not a document, got %T", c)

		cd := collDiffResult{
			Name: cm["name"].(string),
		}

		if addedRaw, ok := cm["added"].(bson.A); ok {
			for _, a := range addedRaw {
				cd.Added = append(cd.Added, a.(bson.M))
			}
		}

		if removedRaw, ok := cm["removed"].(bson.A); ok {
			for _, r := range removedRaw {
				cd.Removed = append(cd.Removed, r.(bson.M))
			}
		}

		if modRaw, ok := cm["modified"].(bson.A); ok {
			for _, m := range modRaw {
				mm, ok := m.(bson.M)
				require.True(t, ok, "modified entry is not a document, got %T", m)

				md := modifiedDocResult{ID: mm["_id"]}

				if diffArr, ok := mm["diff"].(bson.A); ok {
					for _, d := range diffArr {
						dm, ok := d.(bson.M)
						require.True(t, ok, "diff entry is not a document, got %T", d)

						fd := fieldDiffResult{
							Type: fmt.Sprintf("%v", dm["type"]),
							Path: fmt.Sprintf("%v", dm["path"]),
							A:    dm["a"],
							B:    dm["b"],
						}
						md.Diff = append(md.Diff, fd)
					}
				}

				cd.Modified = append(cd.Modified, md)
			}
		}

		out.Collections = append(out.Collections, cd)
	}

	return out
}

// findCollDiff returns the collDiffResult for the named collection, or nil.
func findCollDiff(dr diffResult, name string) *collDiffResult {
	for i := range dr.Collections {
		if dr.Collections[i].Name == name {
			return &dr.Collections[i]
		}
	}

	return nil
}

// findFieldDiffResult returns the fieldDiffResult for the given path within a
// modifiedDocResult, or nil if not found.
func findFieldDiffResult(md modifiedDocResult, path string) *fieldDiffResult {
	for i := range md.Diff {
		if md.Diff[i].Path == path {
			return &md.Diff[i]
		}
	}

	return nil
}

// diffVerifySetup mirrors the Setup section of docs/verify/diff.md.
// Returns hashBase (the baseline commit hash).
func diffVerifySetup(t *testing.T, env *dongoTestEnv, dbName string) (hashBase string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	// Start fresh.
	require.NoError(t, db.Drop(ctx))

	// Baseline: two documents, committed.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
		{Key: "score", Value: int32(10)},
	})
	require.NoError(t, err)

	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
		{Key: "score", Value: int32(20)},
	})
	require.NoError(t, err)

	hashBase = dongoCommit(t, env, dbName, "baseline")

	// Three working-set changes (NOT committed):
	//   _id:3 added
	//   _id:1 modified (score 10 → 99)
	//   _id:2 deleted
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "label", Value: "gamma"},
		{Key: "score", Value: int32(30)},
	})
	require.NoError(t, err)

	_, err = items.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(99)}}}},
	)
	require.NoError(t, err)

	_, err = items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)

	return hashBase
}

func TestDiffVerify(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("diffvrfy%d", rand.Int64N(1_000_000))

	hashBase := diffVerifySetup(t, env, dbName)

	// hashNew is set by Scenario 2 and used by Scenarios 3 and 4.
	var hashNew string

	// -------------------------------------------------------------------------
	// Scenario 1: Working set vs HEAD (default diff, no from/to)
	// -------------------------------------------------------------------------
	t.Run("Scenario1_WorkingSetVsHEAD", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)

		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection; got collections: %v",
			func() []string {
				names := make([]string, len(dr.Collections))
				for i, c := range dr.Collections {
					names[i] = c.Name
				}
				return names
			}())

		// added: exactly _id:3
		require.Len(t, cd.Added, 1, "expected 1 added doc")
		assert.Equal(t, int32(3), cd.Added[0]["_id"], "added doc must be _id:3")

		// removed: exactly _id:2
		require.Len(t, cd.Removed, 1, "expected 1 removed doc")
		assert.Equal(t, int32(2), cd.Removed[0]["_id"], "removed doc must be _id:2")

		// modified: exactly _id:1
		require.Len(t, cd.Modified, 1, "expected 1 modified doc")
		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID, "modified doc must be _id:1")

		// $.score must be the only diff entry (label was not changed)
		scoreDiff := findFieldDiffResult(mod, "$.score")
		require.NotNil(t, scoreDiff, "$.score must appear in modified diff")
		assert.Equal(t, "modified", scoreDiff.Type)
		assert.Equal(t, int32(10), scoreDiff.A, "$.score a (old) must be 10")
		assert.Equal(t, int32(99), scoreDiff.B, "$.score b (new) must be 99")

		// label must NOT appear (unchanged field)
		labelDiff := findFieldDiffResult(mod, "$.label")
		assert.Nil(t, labelDiff, "unchanged field '$.label' must not appear in modified diff")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Commit the working set, then diff two hashes
	// -------------------------------------------------------------------------
	t.Run("Scenario2_DiffTwoHashes", func(t *testing.T) {
		// Commit the working-set changes.
		hashNew = dongoCommit(t, env, dbName, "three changes")
		require.NotEmpty(t, hashNew)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoDiff", Value: int32(1)},
			{Key: "from", Value: hashBase},
			{Key: "to", Value: hashNew},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)

		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection")

		// Same three changes now between two committed snapshots.
		require.Len(t, cd.Added, 1, "expected 1 added doc")
		assert.Equal(t, int32(3), cd.Added[0]["_id"])

		require.Len(t, cd.Removed, 1, "expected 1 removed doc")
		assert.Equal(t, int32(2), cd.Removed[0]["_id"])

		require.Len(t, cd.Modified, 1, "expected 1 modified doc")
		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID)

		scoreDiff := findFieldDiffResult(mod, "$.score")
		require.NotNil(t, scoreDiff, "$.score must appear in modified diff")
		assert.Equal(t, "modified", scoreDiff.Type)
		assert.Equal(t, int32(10), scoreDiff.A)
		assert.Equal(t, int32(99), scoreDiff.B)
	})

	// -------------------------------------------------------------------------
	// Scenario 3: No changes — diff returns an empty collections array
	// -------------------------------------------------------------------------
	t.Run("Scenario3_NoChanges", func(t *testing.T) {
		// After committing in Scenario 2, working set matches HEAD.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		assert.Empty(t, dr.Collections, "expected empty collections after commit with no new changes")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: `from` only — diff from hashBase to current working set
	// -------------------------------------------------------------------------
	t.Run("Scenario4_FromOnly", func(t *testing.T) {
		// Make one more uncommitted change.
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(4)},
			{Key: "label", Value: "delta"},
			{Key: "score", Value: int32(40)},
		})
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoDiff", Value: int32(1)},
			{Key: "from", Value: hashBase},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)

		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection")

		// Two added documents: _id:3 (committed in Scenario 2) and _id:4 (in working set).
		require.Len(t, cd.Added, 2, "expected 2 added docs (_id:3 and _id:4)")

		addedIDs := make(map[any]bool)
		for _, a := range cd.Added {
			addedIDs[a["_id"]] = true
		}

		assert.True(t, addedIDs[int32(3)], "_id:3 must appear in added")
		assert.True(t, addedIDs[int32(4)], "_id:4 must appear in added")

		// One removed document: _id:2 (committed in Scenario 2).
		require.Len(t, cd.Removed, 1, "expected 1 removed doc (_id:2)")
		assert.Equal(t, int32(2), cd.Removed[0]["_id"])

		// One modified document: _id:1 score 10→99 (committed in Scenario 2).
		require.Len(t, cd.Modified, 1, "expected 1 modified doc (_id:1)")
		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID)

		scoreDiff := findFieldDiffResult(mod, "$.score")
		require.NotNil(t, scoreDiff, "$.score must appear in modified diff")
		assert.Equal(t, "modified", scoreDiff.Type)
		assert.Equal(t, int32(10), scoreDiff.A)
		assert.Equal(t, int32(99), scoreDiff.B)
	})
}
