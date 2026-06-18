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

// TestLogPaginationVerify is the automated analog of
// docs/verify/log-pagination-filtering.md. It covers frontier pagination:
// from-array seeds, the next frontier, and the all flag. Commits are made
// sequentially so later commits carry later timestamps, which (with
// height-primary ordering) reproduces the documented walk order
// deterministically. Commit filtering is not yet shipped.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestLogPaginationVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// -------------------------------------------------------------------------
	// Part A: frontier pagination over a dormant-branch DAG.
	//
	//   init - m1 - m2 - m3 - m4 - m5 - M
	//               \                   /
	//                f1 ------- f2 ------
	//
	// Confirmed walk order: M m5 m4 f2 m3 f1 m2 m1 init.
	// -------------------------------------------------------------------------
	pgdb := fmt.Sprintf("logpage%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(pgdb).Drop(ctx))

	label := map[string]string{} // hash -> label
	main := env.client.Database(pgdb)
	feat := env.client.Database(pgdb + "@feat")

	insMain := func(id int32) {
		_, err := main.Collection("coll").InsertOne(ctx, bson.D{{Key: "_id", Value: id}})
		require.NoError(t, err)
	}
	insFeat := func(id int32) {
		_, err := feat.Collection("coll").InsertOne(ctx, bson.D{{Key: "_id", Value: id}})
		require.NoError(t, err)
	}

	insMain(1)
	label[dumboDBCommit(t, env, pgdb, "m1", "a <a@x.io>")] = "m1"
	insMain(2)
	label[dumboDBCommit(t, env, pgdb, "m2", "a <a@x.io>")] = "m2"

	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feat"}}).Err())
	insFeat(101)
	label[dumboDBCommit(t, env, pgdb+"@feat", "f1", "a <a@x.io>")] = "f1"
	insFeat(102)
	label[dumboDBCommit(t, env, pgdb+"@feat", "f2", "a <a@x.io>")] = "f2"

	insMain(3)
	label[dumboDBCommit(t, env, pgdb, "m3", "a <a@x.io>")] = "m3"
	insMain(4)
	label[dumboDBCommit(t, env, pgdb, "m4", "a <a@x.io>")] = "m4"
	insMain(5)
	label[dumboDBCommit(t, env, pgdb, "m5", "a <a@x.io>")] = "m5"

	var mergeRaw bson.M
	require.NoError(t, main.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)}, {Key: "merge_in", Value: "feat"},
	}).Decode(&mergeRaw))
	label[mergeRaw["commitId"].(string)] = "M"

	labelOf := func(hashes []string) []string {
		out := make([]string, len(hashes))
		for i, h := range hashes {
			if l, ok := label[h]; ok {
				out[i] = l
			} else {
				out[i] = "init"
			}
		}
		return out
	}

	wantFull := []string{"M", "m5", "m4", "f2", "m3", "f1", "m2", "m1", "init"}

	t.Run("A1_FullWalkNoNext", func(t *testing.T) {
		raw := runLog(t, env, pgdb, bson.D{{Key: "limit", Value: int32(100)}})
		assert.Equal(t, wantFull, labelOf(logCommitIDs(t, raw)))
		_, hasNext := raw["next"]
		assert.False(t, hasNext, "full walk must omit next")
	})

	t.Run("A2_Page1CarriesDormantTip", func(t *testing.T) {
		raw := runLog(t, env, pgdb, bson.D{{Key: "limit", Value: int32(2)}})
		assert.Equal(t, []string{"M", "m5"}, labelOf(logCommitIDs(t, raw)))
		assert.ElementsMatch(t, []string{"f2", "m4"}, labelOf(logNext(t, raw)),
			"page1 next must carry the dormant feature tip f2 alongside m4")
	})

	t.Run("A3_PageThroughReassembles", func(t *testing.T) {
		var all []string
		seen := map[string]bool{}
		var from bson.A
		for i := 0; ; i++ {
			require.Less(t, i, 100)
			extra := bson.D{{Key: "limit", Value: int32(2)}}
			if from != nil {
				extra = append(extra, bson.E{Key: "from", Value: from})
			}
			raw := runLog(t, env, pgdb, extra)
			for _, id := range logCommitIDs(t, raw) {
				require.False(t, seen[id], "commit emitted twice")
				seen[id] = true
				all = append(all, id)
			}
			next := logNext(t, raw)
			if len(next) == 0 {
				break
			}
			from = bson.A{}
			for _, h := range next {
				from = append(from, h)
			}
		}
		assert.Equal(t, wantFull, labelOf(all), "paged walk must reassemble the full walk exactly")
	})

	t.Run("A4_FromArrayReproducesPage2", func(t *testing.T) {
		// Seed with page 2's frontier {f2, m4}; expect [m4, f2] then next {m3,f1}.
		var f2, m4 string
		for h, l := range label {
			switch l {
			case "f2":
				f2 = h
			case "m4":
				m4 = h
			}
		}
		raw := runLog(t, env, pgdb, bson.D{
			{Key: "limit", Value: int32(2)},
			{Key: "from", Value: bson.A{f2, m4}},
		})
		assert.Equal(t, []string{"m4", "f2"}, labelOf(logCommitIDs(t, raw)))
		assert.ElementsMatch(t, []string{"m3", "f1"}, labelOf(logNext(t, raw)))
	})

	t.Run("A5_SingleHashFromBackCompat", func(t *testing.T) {
		var f2 string
		for h, l := range label {
			if l == "f2" {
				f2 = h
			}
		}
		raw := runLog(t, env, pgdb, bson.D{{Key: "from", Value: f2}})
		assert.Equal(t, []string{"f2", "f1", "m2", "m1", "init"}, labelOf(logCommitIDs(t, raw)))
	})

	t.Run("A6_AllSpansBranches", func(t *testing.T) {
		// A side branch off main with an un-merged commit.
		require.NoError(t, env.client.Database(pgdb+"@main").RunCommand(ctx,
			bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "side"}}).Err())
		_, err := env.client.Database(pgdb+"@side").Collection("coll").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(900)}})
		require.NoError(t, err)
		s1 := dumboDBCommit(t, env, pgdb+"@side", "s1", "a <a@x.io>")

		def := logCommitIDs(t, runLog(t, env, pgdb, bson.D{}))
		assert.NotContains(t, def, s1, "default main walk excludes the side-only commit")

		all := logCommitIDs(t, runLog(t, env, pgdb, bson.D{{Key: "all", Value: true}}))
		assert.Contains(t, all, s1, "all walk includes the side-branch commit")

		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "all", Value: true}, {Key: "from", Value: s1}}
		require.Error(t, env.client.Database(pgdb).RunCommand(ctx, cmd).Err(), "all + from must be rejected")
	})

	// -------------------------------------------------------------------------
	// Part B: collection:_id filtering.
	// -------------------------------------------------------------------------
	fdb := fmt.Sprintf("logfiltv%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(fdb).Drop(ctx))
	fb := env.client.Database(fdb)

	_, err := fb.Collection("orders").InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "status", Value: "pending"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "status", Value: "shipped"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, fdb, "c1 add orders 1,2", "a <a@x.io>")
	_, err = fb.Collection("users").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alice"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, fdb, "c2 add user 1", "a <a@x.io>")
	_, err = fb.Collection("orders").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "status", Value: "pending"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, fdb, "c3 add order 3", "a <a@x.io>")
	_, err = fb.Collection("orders").UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "x"}}}})
	require.NoError(t, err)
	_, err = fb.Collection("orders").UpdateByID(ctx, int32(2), bson.D{{Key: "$set", Value: bson.D{{Key: "region", Value: "eu"}}}})
	require.NoError(t, err)
	_, err = fb.Collection("users").UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "alicia"}}}})
	require.NoError(t, err)
	c4 := dumboDBCommit(t, env, fdb, "c4 mixed edit", "a <a@x.io>")
	_, err = fb.Collection("orders").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, fdb, "c5 delete order 1", "a <a@x.io>")

	fmsgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("B1_FollowOneDocument", func(t *testing.T) {
		raw := runLog(t, env, fdb, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "orders", Value: int32(1)}}}}})
		assert.Equal(t, []string{"c5 delete order 1", "c4 mixed edit", "c1 add orders 1,2"}, fmsgs(raw))
	})

	t.Run("B2_IDListAndOR", func(t *testing.T) {
		raw := runLog(t, env, fdb, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{int32(1), int32(3)}}},
		}}})
		assert.Equal(t, []string{"c5 delete order 1", "c4 mixed edit", "c3 add order 3", "c1 add orders 1,2"}, fmsgs(raw))

		raw = runLog(t, env, fdb, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: int32(3)}}, bson.D{{Key: "users", Value: int32(1)}},
		}}})
		assert.Equal(t, []string{"c4 mixed edit", "c3 add order 3", "c2 add user 1"}, fmsgs(raw))
	})

	t.Run("B2b_WholeCollection", func(t *testing.T) {
		// Bare collection-name string = any _id in orders: every orders commit.
		raw := runLog(t, env, fdb, bson.D{{Key: "filters", Value: bson.A{"orders"}}})
		assert.Equal(t, []string{
			"c5 delete order 1", "c4 mixed edit", "c3 add order 3", "c1 add orders 1,2",
		}, fmsgs(raw))
	})

	t.Run("B3_ScopedPatch", func(t *testing.T) {
		raw := runLog(t, env, fdb, bson.D{
			{Key: "from", Value: c4}, {Key: "limit", Value: int32(1)},
			{Key: "patch", Value: true}, {Key: "filters", Value: bson.A{bson.D{{Key: "orders", Value: int32(1)}}}},
		})
		diff := raw["commits"].(bson.A)[0].(bson.M)["diff"].(bson.A)
		require.Len(t, diff, 1, "scoped to orders only")
		assert.Equal(t, "orders", diff[0].(bson.M)["name"])
		mods := diff[0].(bson.M)["modified"].(bson.A)
		require.Len(t, mods, 1, "only order 1, not order 2")
		assert.EqualValues(t, 1, mods[0].(bson.M)["_id"])
	})

	t.Run("B4_Errors", func(t *testing.T) {
		// not an array
		require.Error(t, env.client.Database(fdb).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.D{{Key: "orders", Value: int32(1)}}},
		}).Err())
		// id-list element that is itself an array (arrays are never valid _ids)
		require.Error(t, env.client.Database(fdb).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
				bson.D{{Key: "orders", Value: bson.A{bson.A{int32(1)}}}},
			}},
		}).Err())
	})

	// Scenario B5: non-integer _ids (ObjectId and document). Fresh database.
	nfdb := fmt.Sprintf("logfiltids%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(nfdb).Drop(ctx))
	nf := env.client.Database(nfdb)
	oid := bson.NewObjectID()
	subID := bson.D{{Key: "region", Value: "us"}, {Key: "seq", Value: int32(5)}}

	_, err = nf.Collection("events").InsertOne(ctx, bson.D{{Key: "_id", Value: oid}, {Key: "kind", Value: "login"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, nfdb, "e1 add oid event", "a <a@x.io>")
	_, err = nf.Collection("events").InsertOne(ctx, bson.D{{Key: "_id", Value: subID}, {Key: "kind", Value: "order"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, nfdb, "e2 add subdoc event", "a <a@x.io>")
	_, err = nf.Collection("events").UpdateOne(ctx, bson.D{{Key: "_id", Value: oid}}, bson.D{{Key: "$set", Value: bson.D{{Key: "kind", Value: "logout"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, nfdb, "e3 modify oid event", "a <a@x.io>")

	nmsgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("B5_ObjectIDFilter", func(t *testing.T) {
		raw := runLog(t, env, nfdb, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "events", Value: oid}}}}})
		assert.Equal(t, []string{"e3 modify oid event", "e1 add oid event"}, nmsgs(raw))
	})

	t.Run("B5_DocumentIDFilter", func(t *testing.T) {
		raw := runLog(t, env, nfdb, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "events", Value: subID}}}}})
		assert.Equal(t, []string{"e2 add subdoc event"}, nmsgs(raw))
	})

	t.Run("B5_DocumentIDFieldOrderSignificant", func(t *testing.T) {
		// Same fields, different order -> a different _id -> matches nothing.
		raw := runLog(t, env, nfdb, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "events", Value: bson.D{{Key: "seq", Value: int32(5)}, {Key: "region", Value: "us"}}}},
		}}})
		assert.Empty(t, raw["commits"].(bson.A))
	})

	// Scenario B6: $match resolved once at HEAD. Fresh database.
	mfdb := fmt.Sprintf("logfiltmatch%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(mfdb).Drop(ctx))
	mf := env.client.Database(mfdb)
	_, err = mf.Collection("orders").InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "status", Value: "pending"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "status", Value: "shipped"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, mfdb, "m1 add 1,2", "a <a@x.io>")
	_, err = mf.Collection("orders").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "status", Value: "pending"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, mfdb, "m2 add 3", "a <a@x.io>")
	_, err = mf.Collection("orders").UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "shipped"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, mfdb, "m3 ship 1", "a <a@x.io>")

	mmsgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("B6_MatchResolvedAtHead", func(t *testing.T) {
		// At HEAD only order 3 is pending; its history is m2.
		raw := runLog(t, env, mfdb, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "pending"}}}}}}},
		}}})
		assert.Equal(t, []string{"m2 add 3"}, mmsgs(raw))
	})

	t.Run("B6_MultipleMatchOR", func(t *testing.T) {
		raw := runLog(t, env, mfdb, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "pending"}}}},
				bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "shipped"}}}},
			}}},
		}}})
		assert.Equal(t, []string{"m3 ship 1", "m2 add 3", "m1 add 1,2"}, mmsgs(raw))
	})
}
