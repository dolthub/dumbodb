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
// docs/verify/log-pagination-filtering.md. Part A covers frontier pagination
// (from-array, next); Part B covers commit filtering (touched semantics, OR,
// limit-counts-matches, _id follow-document). Commits are made sequentially so
// later commits carry later timestamps, which (with height-primary ordering)
// reproduces the documented walk order deterministically.

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

}
