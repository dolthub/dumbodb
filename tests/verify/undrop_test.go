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

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const undropPreservedDir = ".dumbodb_dropped_databases"

func undropCommit(t *testing.T, env *dumboDBTestEnv, dbName string, doc bson.D, msg string) {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(dbName)
	_, err := db.Collection("items").InsertOne(ctx, doc)
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1},
		{Key: "message", Value: msg},
		{Key: "author", Value: "a <a@a>"},
	}).Err())
}

func undropDropDB(t *testing.T, env *dumboDBTestEnv, dbName string) {
	t.Helper()
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(),
		bson.D{{Key: "dropDatabase", Value: 1}}).Err())
}

func undropAdmin(t *testing.T, env *dumboDBTestEnv, cmd bson.D) (bson.M, error) {
	t.Helper()
	var res bson.M
	err := env.Client.Database("admin").RunCommand(context.Background(), cmd).Decode(&res)
	return res, err
}

func undropCommitCount(t *testing.T, env *dumboDBTestEnv, dbName string) int {
	t.Helper()
	var log bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(context.Background(),
		bson.D{{Key: "doltLog", Value: 1}}).Decode(&log))
	return len(log["commits"].(bson.A))
}

func droppedNames(res bson.M) []string {
	var names []string
	for _, d := range res["dropped"].(bson.A) {
		names = append(names, d.(bson.M)["name"].(string))
	}
	return names
}

// TestUndropVerify mirrors docs/verify/undrop.md.
func TestUndropVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Setup: undropvdb with two commits.
	undropCommit(t, env, "undropvdb", bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: "alpha"}}, "commit one")
	undropCommit(t, env, "undropvdb", bson.D{{Key: "_id", Value: 2}, {Key: "label", Value: "beta"}}, "commit two")
	commitCountN := undropCommitCount(t, env, "undropvdb")
	require.GreaterOrEqual(t, commitCountN, 2)

	// Scenario 1: dropDatabase soft-deletes (does not destroy).
	t.Run("Scenario1_DropSoftDeletes", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database("undropvdb").RunCommand(ctx,
			bson.D{{Key: "dropDatabase", Value: 1}}).Decode(&res))
		assert.EqualValues(t, "undropvdb", res["dropped"])

		names, err := env.Client.ListDatabaseNames(ctx, bson.D{})
		require.NoError(t, err)
		assert.NotContains(t, names, "undropvdb")

		entries, err := os.ReadDir(filepath.Join(env.DataDir(), undropPreservedDir, "undropvdb"))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "exactly one preserved copy")
	})

	// Scenario 2: List databases available to undrop.
	t.Run("Scenario2_ListDroppable", func(t *testing.T) {
		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		entries := res["dropped"].(bson.A)
		require.Len(t, entries, 1)
		e := entries[0].(bson.M)
		assert.EqualValues(t, "undropvdb", e["name"])
		assert.NotEmpty(t, e["dropId"])
		assert.NotNil(t, e["droppedAt"])
	})

	// Scenario 3: Undrop restores data AND full history.
	t.Run("Scenario3_UndropRestores", func(t *testing.T) {
		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "undropvdb"}})
		require.NoError(t, err)
		assert.EqualValues(t, "undropvdb", res["undropped"])

		count, err := env.Client.Database("undropvdb").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count, "both documents restored")

		assert.Equal(t, commitCountN, undropCommitCount(t, env, "undropvdb"), "full history restored")

		// Restore is a copy: the drop stays in the list after undrop.
		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		assert.Contains(t, droppedNames(list), "undropvdb", "drop remains listed after undrop")
	})

	// Scenario 4: Repeat drops are all kept; with no dropId, undrop restores the
	// most recent drop.
	t.Run("Scenario4_KeepAllMostRecentDefault", func(t *testing.T) {
		undropCommit(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "first"}}, "g1")
		undropDropDB(t, env, "ledger")
		undropCommit(t, env, "ledger", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "second"}}, "g2")
		undropDropDB(t, env, "ledger")

		entries, err := os.ReadDir(filepath.Join(env.DataDir(), undropPreservedDir, "ledger"))
		require.NoError(t, err)
		require.Len(t, entries, 2, "both drops retained")

		// The list is most-recently-dropped first, so [0] is the "second" drop.
		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		var ledgerDrops []bson.M
		for _, d := range list["dropped"].(bson.A) {
			if dm := d.(bson.M); dm["name"] == "ledger" {
				ledgerDrops = append(ledgerDrops, dm)
			}
		}
		require.Len(t, ledgerDrops, 2)

		// No dropId: restores the MOST RECENT drop ("second").
		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}})
		require.NoError(t, err)
		assert.EqualValues(t, ledgerDrops[0]["dropId"], res["dropId"], "no dropId restores the most recent drop")

		var got bson.M
		require.NoError(t, env.Client.Database("ledger").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "second", got["gen"], "most recent copy restored")

		// Restore is a copy: both drops remain preserved (nothing is consumed).
		remaining, err := os.ReadDir(filepath.Join(env.DataDir(), undropPreservedDir, "ledger"))
		require.NoError(t, err)
		assert.Len(t, remaining, 2, "both drops remain preserved after a copy-restore")
	})

	// Scenario 4b: A specific, non-latest drop can be restored by dropId.
	// Three drops of "journal" exist simultaneously; restoring the MIDDLE one
	// by dropId proves selection is by id, not "most recent" or "oldest".
	t.Run("Scenario4b_RestoreSpecificNonLatest", func(t *testing.T) {
		undropCommit(t, env, "journal", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "v1"}}, "j1")
		undropDropDB(t, env, "journal")
		undropCommit(t, env, "journal", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "v2"}}, "j2")
		undropDropDB(t, env, "journal")
		undropCommit(t, env, "journal", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "v3"}}, "j3")
		undropDropDB(t, env, "journal")

		// Most-recently-dropped first: [v3, v2, v1]. The middle entry is v2.
		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		var journalDrops []bson.M
		for _, d := range list["dropped"].(bson.A) {
			if dm := d.(bson.M); dm["name"] == "journal" {
				journalDrops = append(journalDrops, dm)
			}
		}
		require.Len(t, journalDrops, 3)
		middleDropID := journalDrops[1]["dropId"].(string)

		// Restore the middle drop by its dropId (neither newest nor oldest).
		res, err := undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "journal"}, {Key: "dropId", Value: middleDropID},
		})
		require.NoError(t, err)
		assert.EqualValues(t, middleDropID, res["dropId"], "the requested dropId was restored")

		var got bson.M
		require.NoError(t, env.Client.Database("journal").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "v2", got["gen"], "the specific non-latest copy (v2) was restored")

		// Restore is a copy: all three drops (v1, v2, v3) remain preserved.
		remaining, err := os.ReadDir(filepath.Join(env.DataDir(), undropPreservedDir, "journal"))
		require.NoError(t, err)
		assert.Len(t, remaining, 3, "all drops remain preserved after a copy-restore")
	})

	// Scenario 4c: Restore under a different name with to_database, repeatedly.
	// Because restore is a copy, one drop can seed several live copies.
	t.Run("Scenario4c_ToDatabaseMultipleCopies", func(t *testing.T) {
		undropCommit(t, env, "srcdb", bson.D{{Key: "_id", Value: 1}, {Key: "tag", Value: "orig"}}, "s1")
		undropDropDB(t, env, "srcdb")

		// First copy.
		res, err := undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "srcdb"}, {Key: "to_database", Value: "destdb"},
		})
		require.NoError(t, err)
		assert.EqualValues(t, "destdb", res["undropped"], "restored under the alternate name")

		var got bson.M
		require.NoError(t, env.Client.Database("destdb").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "orig", got["tag"])

		// srcdb was NOT consumed: it is still listed as a drop.
		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		assert.Contains(t, droppedNames(list), "srcdb", "drop remains after a to_database restore")

		// Second copy from the same drop, under another name.
		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "srcdb"}, {Key: "to_database", Value: "destdb2"},
		})
		require.NoError(t, err)
		require.NoError(t, env.Client.Database("destdb2").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "orig", got["tag"], "second independent copy has the data too")

		// The two copies are independent: mutating one does not affect the other.
		_, err = env.Client.Database("destdb").Collection("items").
			UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "tag", Value: "changed"}}}})
		require.NoError(t, err)
		require.NoError(t, env.Client.Database("destdb2").Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "orig", got["tag"], "destdb2 is unaffected by writes to destdb")

		// srcdb still listed after the second copy.
		list, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		assert.Contains(t, droppedNames(list), "srcdb")
	})

	// Scenario 4d: a name with an all-digit "@" suffix is a valid plain database
	// name (the digits are not a revision), so it can be dropped AND undropped.
	t.Run("Scenario4d_AllDigitAtSuffixName", func(t *testing.T) {
		const name = "parity_test@1783469187442119000"
		undropCommit(t, env, name, bson.D{{Key: "_id", Value: 1}, {Key: "tag", Value: "x"}}, "c1")
		undropDropDB(t, env, name)

		list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
		require.NoError(t, err)
		require.Contains(t, droppedNames(list), name, "the @-digit name is listed as a drop")

		res, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: name}})
		require.NoError(t, err, "an @-digit name must be undroppable, not just droppable")
		assert.EqualValues(t, name, res["undropped"])

		var got bson.M
		require.NoError(t, env.Client.Database(name).Collection("items").
			FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
		assert.EqualValues(t, "x", got["tag"])
	})

	// Scenario 5: Error cases.
	t.Run("Scenario5_Errors", func(t *testing.T) {
		_, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ghost"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "no dropped database")

		// ledger is live again from Scenario 4.
		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "already exists")

		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger@main"}})
		require.Error(t, err)

		// to_database without name is an error.
		_, err = undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "to_database", Value: "somewhere"}})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "to_database requires name")

		// to_database must be a root name.
		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}, {Key: "to_database", Value: "dest@main"},
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "to_database must be a root database")

		// Cannot restore onto reserved system databases config/local.
		for _, sys := range []string{"config", "local"} {
			_, err = undropAdmin(t, env, bson.D{
				{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "ledger"}, {Key: "to_database", Value: sys},
			})
			require.Error(t, err, "undrop to %q must be rejected", sys)
			assert.Contains(t, strings.ToLower(err.Error()), "system database")
		}
	})

	// Scenario 6: undrop is admin-only.
	t.Run("Scenario6_AdminOnly", func(t *testing.T) {
		err := env.Client.Database("undropvdb").RunCommand(ctx,
			bson.D{{Key: "dumboUndrop", Value: 1}, {Key: "name", Value: "undropvdb"}}).Err()
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "admin database")
	})

	// Scenario 7: System databases cannot be dropped.
	t.Run("Scenario7_SystemDbsProtected", func(t *testing.T) {
		for _, sys := range []string{"admin", "config", "local"} {
			err := env.Client.Database(sys).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}}).Err()
			require.Error(t, err, "dropping %q must error", sys)
			assert.Contains(t, strings.ToLower(err.Error()), "system databases cannot be dropped")
		}
		_, statErr := os.Stat(filepath.Join(env.DataDir(), "admin"))
		assert.NoError(t, statErr, "admin dir survives")
	})

	// Scenario 7b: config and local cannot be created by users. DumboDB does not
	// implement their MongoDB semantics, so materializing them is refused.
	t.Run("Scenario7b_ReservedDbsCannotBeCreated", func(t *testing.T) {
		for _, sys := range []string{"config", "local"} {
			_, err := env.Client.Database(sys).Collection("c").
				InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
			require.Error(t, err, "creating %q must be rejected", sys)
			assert.Contains(t, strings.ToLower(err.Error()), "namespace")

			_, statErr := os.Stat(filepath.Join(env.DataDir(), sys))
			assert.True(t, os.IsNotExist(statErr), "%q dir must not be created", sys)
		}

		// admin remains usable (positive control): a write to admin succeeds.
		_, err := env.Client.Database("admin").Collection("probe").
			InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
		require.NoError(t, err, "admin must remain writable")
	})

	// Scenario 8: purgeMatching removes selected preserved drops before the GC.
	// name is required; dropId and droppedBefore further narrow the match.
	t.Run("Scenario8_PurgeMatching", func(t *testing.T) {
		// Seed: pa dropped twice, pb once.
		for _, n := range []string{"pa", "pb"} {
			undropCommit(t, env, n, bson.D{{Key: "_id", Value: 1}}, "c1")
			undropDropDB(t, env, n)
		}
		undropCommit(t, env, "pa", bson.D{{Key: "_id", Value: 2}}, "c2")
		undropDropDB(t, env, "pa")

		paDrops := preservedDropsFor(t, env, "pa")
		require.Len(t, paDrops, 2)

		// Purge one exact drop by name + dropId.
		oneID := paDrops[0]["dropId"].(string)
		res, err := undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{{Key: "name", Value: "pa"}, {Key: "dropId", Value: oneID}}},
		})
		require.NoError(t, err)
		purged := res["purged"].(bson.A)
		require.Len(t, purged, 1)
		assert.EqualValues(t, oneID, purged[0].(bson.M)["dropId"])
		require.Len(t, preservedDropsFor(t, env, "pa"), 1, "one pa drop remains")

		// Purge all remaining drops of a name.
		res, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{{Key: "name", Value: "pa"}}},
		})
		require.NoError(t, err)
		assert.Len(t, res["purged"].(bson.A), 1)
		assert.Empty(t, preservedDropsFor(t, env, "pa"), "pa fully purged")
		assert.Len(t, preservedDropsFor(t, env, "pb"), 1, "pb untouched (purge is name-scoped)")

		// name + droppedBefore, cutoff in the past: matches nothing.
		res, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{
				{Key: "name", Value: "pb"},
				{Key: "droppedBefore", Value: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
			}},
		})
		require.NoError(t, err)
		assert.Empty(t, res["purged"].(bson.A), "pb is not older than year 2000")
		assert.Len(t, preservedDropsFor(t, env, "pb"), 1, "pb still preserved")

		// name + droppedBefore, cutoff in the future: purges the matching drop.
		res, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{
				{Key: "name", Value: "pb"},
				{Key: "droppedBefore", Value: time.Now().Add(time.Hour)},
			}},
		})
		require.NoError(t, err)
		assert.Len(t, res["purged"].(bson.A), 1)
		assert.Empty(t, preservedDropsFor(t, env, "pb"))
	})

	// Scenario 8c: droppedBefore is the discriminator -- of two drops of one name,
	// only the one dropped before the cutoff is purged; the newer one is kept.
	t.Run("Scenario8c_PurgeByDroppedBefore", func(t *testing.T) {
		// Drop "cutoffdb", capture a cutoff, then drop it again. The sleeps give
		// the two drops (and the cutoff) millisecond separation so the cutoff
		// falls strictly between them.
		undropCommit(t, env, "cutoffdb", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "old"}}, "c1")
		undropDropDB(t, env, "cutoffdb")
		time.Sleep(10 * time.Millisecond)
		cutoff := time.Now()
		time.Sleep(10 * time.Millisecond)
		undropCommit(t, env, "cutoffdb", bson.D{{Key: "_id", Value: 1}, {Key: "gen", Value: "new"}}, "c2")
		undropDropDB(t, env, "cutoffdb")

		// Two drops now exist: [newer, older] (most-recent first).
		drops := preservedDropsFor(t, env, "cutoffdb")
		require.Len(t, drops, 2)
		newerID := drops[0]["dropId"].(string)
		olderID := drops[1]["dropId"].(string)

		res, err := undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{
				{Key: "name", Value: "cutoffdb"},
				{Key: "droppedBefore", Value: cutoff},
			}},
		})
		require.NoError(t, err)

		// Only the older drop (dropped before the cutoff) is purged.
		purged := res["purged"].(bson.A)
		require.Len(t, purged, 1, "only the pre-cutoff drop is purged")
		assert.EqualValues(t, olderID, purged[0].(bson.M)["dropId"])

		// The newer drop (dropped after the cutoff) survives.
		remaining := preservedDropsFor(t, env, "cutoffdb")
		require.Len(t, remaining, 1)
		assert.Equal(t, newerID, remaining[0]["dropId"].(string), "the post-cutoff drop is kept")
	})

	// Scenario 8b: purgeMatching validation.
	t.Run("Scenario8b_PurgeMatchingErrors", func(t *testing.T) {
		// name is required.
		_, err := undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1}, {Key: "purgeMatching", Value: bson.D{}},
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "requires name")

		// droppedBefore alone (no name) is rejected.
		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{{Key: "droppedBefore", Value: time.Now()}}},
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "requires name")

		// unknown field is rejected (guards against typos like droppedAt).
		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "purgeMatching", Value: bson.D{{Key: "droppedAt", Value: time.Now()}}},
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "unknown field")

		// cannot combine purge with restore params.
		_, err = undropAdmin(t, env, bson.D{
			{Key: "dumboUndrop", Value: 1},
			{Key: "name", Value: "whatever"},
			{Key: "purgeMatching", Value: bson.D{{Key: "name", Value: "whatever"}}},
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "cannot be combined")
	})
}

// preservedDropsFor returns the dumboUndrop list entries for a given db name.
func preservedDropsFor(t *testing.T, env *dumboDBTestEnv, name string) []bson.M {
	t.Helper()
	list, err := undropAdmin(t, env, bson.D{{Key: "dumboUndrop", Value: 1}})
	require.NoError(t, err)
	var out []bson.M
	for _, d := range list["dropped"].(bson.A) {
		if dm := d.(bson.M); dm["name"] == name {
			out = append(out, dm)
		}
	}
	return out
}
