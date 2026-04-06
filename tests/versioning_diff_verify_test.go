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

// diffResult holds the decoded top-level response from a docudoltDiff command.
type diffResult struct {
	Collections []collDiffResult
}

// collDiffResult holds the decoded per-collection section of a docudoltDiff response.
type collDiffResult struct {
	Name     string
	Added    []bson.M
	Removed  []bson.M
	Modified []modifiedDocResult
}

// modifiedDocResult holds one entry from the "modified" list in a docudoltDiff response.
type modifiedDocResult struct {
	ID   any
	Diff []fieldDiffResult
}

// fieldDiffResult holds one entry from the "diff" list of a modified document.
type fieldDiffResult struct {
	Type string
	Path string
	From any
	To   any
}

// decodeDiffResult parses the raw bson.M from a docudoltDiff RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func decodeDiffResult(t *testing.T, raw bson.M) diffResult {
	t.Helper()

	rawColls, ok := raw["collections"]
	require.True(t, ok, "docudoltDiff result missing 'collections' field")

	collsArr, ok := rawColls.(bson.A)
	require.True(t, ok, "docudoltDiff 'collections' is not an array, got %T", rawColls)

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
							From: dm["from"],
							To:   dm["to"],
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
func diffVerifySetup(t *testing.T, env *docudoltTestEnv, dbName string) (hashBase string) {
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

	hashBase = docudoltCommit(t, env, dbName, "baseline")

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
	env := startDocudolt(t)
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
			{Key: "docudoltDiff", Value: int32(1)},
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
		assert.Equal(t, int32(10), scoreDiff.From, "$.score a (old) must be 10")
		assert.Equal(t, int32(99), scoreDiff.To, "$.score b (new) must be 99")

		// label must NOT appear (unchanged field)
		labelDiff := findFieldDiffResult(mod, "$.label")
		assert.Nil(t, labelDiff, "unchanged field '$.label' must not appear in modified diff")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Commit the working set, then diff two hashes
	// -------------------------------------------------------------------------
	t.Run("Scenario2_DiffTwoHashes", func(t *testing.T) {
		// Commit the working-set changes.
		hashNew = docudoltCommit(t, env, dbName, "three changes")
		require.NotEmpty(t, hashNew)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
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
		assert.Equal(t, int32(10), scoreDiff.From)
		assert.Equal(t, int32(99), scoreDiff.To)
	})

	// -------------------------------------------------------------------------
	// Scenario 3: No changes — diff returns an empty collections array
	// -------------------------------------------------------------------------
	t.Run("Scenario3_NoChanges", func(t *testing.T) {
		// After committing in Scenario 2, working set matches HEAD.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
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
			{Key: "docudoltDiff", Value: int32(1)},
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
		assert.Equal(t, int32(10), scoreDiff.From)
		assert.Equal(t, int32(99), scoreDiff.To)
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Multiple documents with mixed changes
	// Delete one, modify one, leave one unchanged, add one — only changed docs appear.
	// -------------------------------------------------------------------------
	t.Run("Scenario5_MultipleDocsWithMixedChanges", func(t *testing.T) {
		multi := env.client.Database(dbName).Collection("multi")

		// Commit 3-doc baseline.
		_, err := multi.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}, {Key: "v", Value: int32(1)}})
		require.NoError(t, err)
		_, err = multi.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "beta"}, {Key: "v", Value: int32(2)}})
		require.NoError(t, err)
		_, err = multi.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "name", Value: "gamma"}, {Key: "v", Value: int32(3)}})
		require.NoError(t, err)
		docudoltCommit(t, env, dbName, "multi baseline")

		// Working set: delete _id:1, modify _id:2 (v only), leave _id:3, add _id:4.
		_, err = multi.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		_, err = multi.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(2)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(99)}}}},
		)
		require.NoError(t, err)
		_, err = multi.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(4)}, {Key: "name", Value: "delta"}, {Key: "v", Value: int32(4)}})
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		cd := findCollDiff(dr, "multi")
		require.NotNil(t, cd, "expected diff for 'multi' collection")

		// removed: exactly _id:1
		require.Len(t, cd.Removed, 1, "expected 1 removed doc")
		assert.Equal(t, int32(1), cd.Removed[0]["_id"])

		// modified: exactly _id:2; $.v changed, $.name unchanged so absent
		require.Len(t, cd.Modified, 1, "expected 1 modified doc (_id:2); _id:3 must not appear")
		mod := cd.Modified[0]
		assert.Equal(t, int32(2), mod.ID)
		vDiff := findFieldDiffResult(mod, "$.v")
		require.NotNil(t, vDiff, "$.v must appear in modified diff")
		assert.Equal(t, "modified", vDiff.Type)
		assert.Equal(t, int32(2), vDiff.From)
		assert.Equal(t, int32(99), vDiff.To)
		assert.Nil(t, findFieldDiffResult(mod, "$.name"), "unchanged $.name must not appear")

		// added: exactly _id:4
		require.Len(t, cd.Added, 1, "expected 1 added doc")
		assert.Equal(t, int32(4), cd.Added[0]["_id"])
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Single document with simultaneous modify + add + remove field ops
	// -------------------------------------------------------------------------
	t.Run("Scenario6_SingleDocMixedFieldOps", func(t *testing.T) {
		// Commit the working set from Scenario 5 first so HEAD is clean.
		docudoltCommit(t, env, dbName, "pre-scenario6")

		mf := env.client.Database(dbName).Collection("mixedfields")

		// Baseline: doc with x and y.
		_, err := mf.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "x", Value: int32(10)},
			{Key: "y", Value: "remove-me"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, dbName, "mixedfields baseline")

		// Replace: x modified, y removed, z added.
		_, err = mf.ReplaceOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{
				{Key: "_id", Value: int32(1)},
				{Key: "x", Value: int32(99)},
				{Key: "z", Value: "new-field"},
			},
		)
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		cd := findCollDiff(dr, "mixedfields")
		require.NotNil(t, cd, "expected diff for 'mixedfields' collection")
		require.Len(t, cd.Modified, 1, "expected 1 modified doc")
		assert.Empty(t, cd.Added, "expected no added docs")
		assert.Empty(t, cd.Removed, "expected no removed docs")

		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID)

		// $.x: modified, a=10, b=99
		xDiff := findFieldDiffResult(mod, "$.x")
		require.NotNil(t, xDiff, "$.x must appear in diff")
		assert.Equal(t, "modified", xDiff.Type)
		assert.Equal(t, int32(10), xDiff.From)
		assert.Equal(t, int32(99), xDiff.To)

		// $.y: removed, a="remove-me", b absent
		yDiff := findFieldDiffResult(mod, "$.y")
		require.NotNil(t, yDiff, "$.y must appear in diff")
		assert.Equal(t, "removed", yDiff.Type)
		assert.Equal(t, "remove-me", yDiff.From)
		assert.Nil(t, yDiff.To, "$.y b must be absent for removed")

		// $.z: added, a absent, b="new-field"
		zDiff := findFieldDiffResult(mod, "$.z")
		require.NotNil(t, zDiff, "$.z must appear in diff")
		assert.Equal(t, "added", zDiff.Type)
		assert.Nil(t, zDiff.From, "$.z a must be absent for added")
		assert.Equal(t, "new-field", zDiff.To)

		// Exactly 3 diff entries.
		assert.Len(t, mod.Diff, 3, "expected exactly 3 field diffs (x modified, y removed, z added)")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Field type change (number → string)
	// -------------------------------------------------------------------------
	t.Run("Scenario7_FieldTypeChange", func(t *testing.T) {
		// Commit working set from Scenario 6 for a clean HEAD.
		docudoltCommit(t, env, dbName, "pre-scenario7")

		tc := env.client.Database(dbName).Collection("typechg")

		// Baseline: "val" is a number.
		_, err := tc.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "val", Value: int32(42)},
			{Key: "stable", Value: "unchanged"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, dbName, "typechg baseline")

		// Replace: val changes from number to string.
		_, err = tc.ReplaceOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{
				{Key: "_id", Value: int32(1)},
				{Key: "val", Value: "forty-two"},
				{Key: "stable", Value: "unchanged"},
			},
		)
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		cd := findCollDiff(dr, "typechg")
		require.NotNil(t, cd, "expected diff for 'typechg' collection")
		require.Len(t, cd.Modified, 1, "expected 1 modified doc")

		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID)

		// $.val: modified, a=int32(42), b="forty-two"
		valDiff := findFieldDiffResult(mod, "$.val")
		require.NotNil(t, valDiff, "$.val must appear in diff")
		assert.Equal(t, "modified", valDiff.Type)
		assert.Equal(t, int32(42), valDiff.From, "$.val a must be the original number")
		assert.Equal(t, "forty-two", valDiff.To, "$.val b must be the new string")

		// $.stable must not appear (unchanged).
		assert.Nil(t, findFieldDiffResult(mod, "$.stable"), "unchanged $.stable must not appear")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Nested document field change
	// Only the changed leaf field ($.address.city) appears; zip and name do not.
	// -------------------------------------------------------------------------
	// -------------------------------------------------------------------------
	// Scenario 9: Rootish expressions in from/to — HEAD, HEAD~N, branch name
	// -------------------------------------------------------------------------
	t.Run("Scenario9_RootishExpressionsInFromTo", func(t *testing.T) {
		// Commit any pending working set so we start from a clean HEAD.
		docudoltCommit(t, env, dbName, "pre-scenario9")

		// Create a feature branch that starts at current main HEAD.
		require.NoError(t, env.client.Database(dbName+"__d_main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "rootishtest"},
		}).Err(), "docudoltBranch to create 'rootishtest'")

		rootish := env.client.Database(dbName + "__d_rootishtest")

		// Two commits on main — feature branch stays behind.
		hashC1 := docudoltCommit(t, env, dbName, "scenario9-c1")

		_, err := env.client.Database(dbName).Collection("scenario9").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: int32(1)},
		})
		require.NoError(t, err)

		hashC2 := docudoltCommit(t, env, dbName, "scenario9-c2")

		// 9a: from=HEAD~1, to=HEAD on main connection — HEAD resolves to c2.
		// Expect _id:1 added (delta between c1 and c2).
		var raw9a bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: "HEAD~1"},
			{Key: "to", Value: "HEAD"},
		}).Decode(&raw9a))

		dr9a := decodeDiffResult(t, raw9a)
		cd9a := findCollDiff(dr9a, "scenario9")
		require.NotNil(t, cd9a, "9a: expected diff for 'scenario9' collection")
		require.Len(t, cd9a.Added, 1, "9a: expected 1 added doc")
		assert.Equal(t, int32(1), cd9a.Added[0]["_id"], "9a: added doc must be _id:1")

		// 9b: from=hashC1, to="HEAD" on main connection — HEAD = c2.
		// Same result as 9a.
		var raw9b bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: hashC1},
			{Key: "to", Value: "HEAD"},
		}).Decode(&raw9b))

		dr9b := decodeDiffResult(t, raw9b)
		cd9b := findCollDiff(dr9b, "scenario9")
		require.NotNil(t, cd9b, "9b: expected diff for 'scenario9' collection")
		require.Len(t, cd9b.Added, 1, "9b: expected 1 added doc")
		assert.Equal(t, int32(1), cd9b.Added[0]["_id"], "9b: added doc must be _id:1")

		// 9c: from=hashC2, to="HEAD" on rootishtest branch connection.
		// HEAD resolves to rootishtest tip = c1 (branch was created before c2),
		// so hashC2 != rootishtest HEAD → expect an error or at least a non-empty diff.
		// Actually hashC2 is AFTER rootishtest was branched, so rootishtest HEAD == hashC1.
		// Diff from hashC2 to rootishtest HEAD: rootishtest has _id:1 absent, hashC2 has it.
		// So _id:1 should appear as removed.
		var raw9c bson.M
		require.NoError(t, rootish.RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: hashC2},
			{Key: "to", Value: "HEAD"},
		}).Decode(&raw9c))

		dr9c := decodeDiffResult(t, raw9c)
		cd9c := findCollDiff(dr9c, "scenario9")
		require.NotNil(t, cd9c, "9c: expected diff for 'scenario9' collection")
		require.Len(t, cd9c.Removed, 1, "9c: expected 1 removed doc (_id:1 absent from rootishtest)")
		assert.Equal(t, int32(1), cd9c.Removed[0]["_id"], "9c: removed doc must be _id:1")

		// 9d: from=hashC1, to="HEAD" on rootishtest branch connection.
		// rootishtest HEAD == c1 == hashC1 → identical → empty diff.
		var raw9d bson.M
		require.NoError(t, rootish.RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: hashC1},
			{Key: "to", Value: "HEAD"},
		}).Decode(&raw9d))

		dr9d := decodeDiffResult(t, raw9d)
		cd9d := findCollDiff(dr9d, "scenario9")
		assert.Nil(t, cd9d, "9d: HEAD on rootishtest resolves to hashC1; diff must be empty, got collection diff")

		// 9e: from="rootishtest" (bare branch name), to="main" — branch name rootish.
		// rootishtest = c1, main = c2: expect _id:1 added.
		var raw9e bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: "rootishtest"},
			{Key: "to", Value: "main"},
		}).Decode(&raw9e))

		dr9e := decodeDiffResult(t, raw9e)
		cd9e := findCollDiff(dr9e, "scenario9")
		require.NotNil(t, cd9e, "9e: expected diff for 'scenario9' collection")
		require.Len(t, cd9e.Added, 1, "9e: expected 1 added doc")
		assert.Equal(t, int32(1), cd9e.Added[0]["_id"], "9e: added doc must be _id:1")

		// 9f: REVERSE — from=HEAD, to=HEAD~1 on main connection.
		// Inverts 9a: _id:1 was added going forward, so going backward it appears as removed.
		var raw9f bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: "HEAD"},
			{Key: "to", Value: "HEAD~1"},
		}).Decode(&raw9f))

		dr9f := decodeDiffResult(t, raw9f)
		cd9f := findCollDiff(dr9f, "scenario9")
		require.NotNil(t, cd9f, "9f: expected diff for 'scenario9' collection")
		assert.Empty(t, cd9f.Added, "9f: reverse diff must have no added docs")
		require.Len(t, cd9f.Removed, 1, "9f: reverse diff must have 1 removed doc (_id:1 absent in HEAD~1)")
		assert.Equal(t, int32(1), cd9f.Removed[0]["_id"], "9f: removed doc must be _id:1")

		// 9g: REVERSE — from="main", to="rootishtest" (bare branch names, reverse of 9e).
		// 9e showed _id:1 added going rootishtest→main; reversed: _id:1 is removed.
		var raw9g bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: "main"},
			{Key: "to", Value: "rootishtest"},
		}).Decode(&raw9g))

		dr9g := decodeDiffResult(t, raw9g)
		cd9g := findCollDiff(dr9g, "scenario9")
		require.NotNil(t, cd9g, "9g: expected diff for 'scenario9' collection")
		assert.Empty(t, cd9g.Added, "9g: reverse diff must have no added docs")
		require.Len(t, cd9g.Removed, 1, "9g: reverse diff must have 1 removed doc (_id:1 absent in rootishtest)")
		assert.Equal(t, int32(1), cd9g.Removed[0]["_id"], "9g: removed doc must be _id:1")

		_ = hashC2 // used above
	})

	t.Run("Scenario8_NestedDocFieldChange", func(t *testing.T) {
		// Commit working set from Scenario 7 for a clean HEAD.
		docudoltCommit(t, env, dbName, "pre-scenario8")

		nested := env.client.Database(dbName).Collection("nested")

		// Baseline: doc with a sub-document address and a top-level name.
		_, err := nested.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "address", Value: bson.D{
				{Key: "city", Value: "Seattle"},
				{Key: "zip", Value: "98101"},
			}},
			{Key: "name", Value: "alice"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, dbName, "nested baseline")

		// Update: only address.city changes.
		_, err = nested.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "address.city", Value: "Portland"}}}},
		)
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		cd := findCollDiff(dr, "nested")
		require.NotNil(t, cd, "expected diff for 'nested' collection")
		require.Len(t, cd.Modified, 1, "expected 1 modified doc")

		mod := cd.Modified[0]
		assert.Equal(t, int32(1), mod.ID)

		// $.address.city: modified, a="Seattle", b="Portland"
		cityDiff := findFieldDiffResult(mod, "$.address.city")
		require.NotNil(t, cityDiff, "$.address.city must appear in diff")
		assert.Equal(t, "modified", cityDiff.Type)
		assert.Equal(t, "Seattle", cityDiff.From)
		assert.Equal(t, "Portland", cityDiff.To)

		// $.address.zip and $.name must not appear (unchanged).
		assert.Nil(t, findFieldDiffResult(mod, "$.address.zip"), "unchanged $.address.zip must not appear")
		assert.Nil(t, findFieldDiffResult(mod, "$.name"), "unchanged $.name must not appear")
	})
}
