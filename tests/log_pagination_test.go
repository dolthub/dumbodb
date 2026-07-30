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

// Handler-level coverage for dumboLog frontier pagination (workspace-tnf.3):
// the "from" parameter accepts a string or an array of hashes, and the
// response carries "next" until the walk is exhausted. Uses a linear history
// so the walk order is unambiguous regardless of commit timestamps.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestLogPaginationHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logpage%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))

	// Linear history: c1..c5 plus the Initialize root => 6 commits total.
	var hashes []string
	for i := 1; i <= 5; i++ {
		_, err := env.Client.Database(dbName).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(i)}})
		require.NoError(t, err)
		hashes = append(hashes, dumboDBCommit(t, env, dbName, fmt.Sprintf("c%d", i), "a <a@x.io>"))
	}
	const total = 6 // 5 user commits + Initialize root

	t.Run("FromAbsent_PagesAndReassembles", func(t *testing.T) {
		var all []string
		seen := map[string]bool{}
		var from bson.A
		pages := 0
		for {
			pages++
			extra := bson.D{{Key: "limit", Value: int32(2)}}
			if from != nil {
				extra = append(extra, bson.E{Key: "from", Value: from})
			}
			raw := runLog(t, env, dbName, extra)
			for _, id := range logCommitIDs(t, raw) {
				require.False(t, seen[id], "commit %s emitted twice", id)
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
			require.Less(t, pages, 100, "pagination did not terminate")
		}
		require.Len(t, all, total, "all pages should cover the whole history exactly once")
	})

	t.Run("NextOmittedWhenExhausted", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "limit", Value: int32(100)}})
		require.Len(t, logCommitIDs(t, raw), total)
		_, hasNext := raw["next"]
		assert.False(t, hasNext, "next must be omitted when the whole history fits")
	})

	t.Run("FromString_BackCompat", func(t *testing.T) {
		// Start at c3 (hashes[2]); reachable: c3, c2, c1, init => 4 commits.
		raw := runLog(t, env, dbName, bson.D{{Key: "from", Value: hashes[2]}})
		ids := logCommitIDs(t, raw)
		require.Len(t, ids, 4)
		assert.Equal(t, hashes[2], ids[0])
	})

	t.Run("FromArray_OneElementEqualsString", func(t *testing.T) {
		rawStr := runLog(t, env, dbName, bson.D{{Key: "from", Value: hashes[2]}})
		rawArr := runLog(t, env, dbName, bson.D{{Key: "from", Value: bson.A{hashes[2]}}})
		assert.Equal(t, logCommitIDs(t, rawStr), logCommitIDs(t, rawArr))
	})

	t.Run("Limit0_EmptyNoNext", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "limit", Value: int32(0)}})
		assert.Empty(t, logCommitIDs(t, raw))
		_, hasNext := raw["next"]
		assert.False(t, hasNext)
	})

	t.Run("InvalidHashElement_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "from", Value: bson.A{"notavalidhash"}}}
		err := env.Client.Database(dbName).RunCommand(ctx, cmd).Err()
		require.Error(t, err)
	})

	t.Run("WrongTypeElement_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "from", Value: bson.A{int32(123)}}}
		err := env.Client.Database(dbName).RunCommand(ctx, cmd).Err()
		require.Error(t, err)
	})
}

// TestLogNegativeLimitRejected verifies a negative limit is rejected rather than
// silently treated as the default limit (workspace-9eg).
func TestLogNegativeLimitRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logneg%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	_, err := env.Client.Database(dbName).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c1", "a <a@x.io>")

	cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(-3)}}
	require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
}

func TestLogAllHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logall%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))

	// main: one commit.
	_, err := env.Client.Database(dbName).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main-1", "a <a@x.io>")

	// side branch with a commit not reachable from main.
	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx,
		bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "side"}}).Err())
	_, err = env.Client.Database(dbName+"@side").Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)
	sideHash := dumboDBCommit(t, env, dbName+"@side", "side-1", "a <a@x.io>")

	t.Run("AllSpansBranches", func(t *testing.T) {
		def := logCommitIDs(t, runLog(t, env, dbName, bson.D{}))
		assert.NotContains(t, def, sideHash, "default main walk excludes side-only commit")

		all := logCommitIDs(t, runLog(t, env, dbName, bson.D{{Key: "all", Value: true}}))
		assert.Contains(t, all, sideHash, "all walk includes the side-branch commit")
	})

	t.Run("AllAndFromMutuallyExclusive", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "all", Value: true}, {Key: "from", Value: sideHash}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})
}

func TestLogIDFilterHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logidf%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	orders := env.Client.Database(dbName).Collection("orders")
	users := env.Client.Database(dbName).Collection("users")

	// c1: orders 1,2 ; c2: users 1 ; c3: order 3 ; c4: modify order 1 + user 1 ; c5: delete order 1
	_, err := orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "status", Value: "pending"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "status", Value: "shipped"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c1", "a <a@x.io>")
	_, err = users.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alice"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c2", "a <a@x.io>")
	_, err = orders.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "status", Value: "pending"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c3", "a <a@x.io>")
	_, err = orders.UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "x"}}}})
	require.NoError(t, err)
	_, err = users.UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "alicia"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c4", "a <a@x.io>")
	_, err = orders.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c5", "a <a@x.io>")

	msgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("FollowOneDocument", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "orders", Value: int32(1)}}}}})
		assert.Equal(t, []string{"c5", "c4", "c1"}, msgs(raw))
	})

	t.Run("IDListSugar", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{int32(1), int32(3)}}},
		}}})
		assert.Equal(t, []string{"c5", "c4", "c3", "c1"}, msgs(raw))
	})

	t.Run("WholeCollection", func(t *testing.T) {
		// Bare collection-name string = any _id in orders: c5, c4, c3, c1.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{"orders"}}})
		assert.Equal(t, []string{"c5", "c4", "c3", "c1"}, msgs(raw))
	})

	t.Run("EmptyArrayRejected", func(t *testing.T) {
		// The old empty-array wildcard is no longer accepted; use the string.
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{}}},
		}}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})

	t.Run("MultiCollectionOR", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: int32(3)}},
			bson.D{{Key: "users", Value: int32(1)}},
		}}})
		assert.Equal(t, []string{"c4", "c3", "c2"}, msgs(raw))
	})

	t.Run("ScopedPatch", func(t *testing.T) {
		// c4 changed order 1 (matched), user 1 (other coll). Scope to orders/1.
		raw := runLog(t, env, dbName, bson.D{
			{Key: "filters", Value: bson.A{bson.D{{Key: "orders", Value: int32(1)}}}},
			{Key: "patch", Value: true}, {Key: "limit", Value: int32(1)},
		})
		changes := raw["commits"].(bson.A)[0].(bson.M)["changes"].(bson.A)
		require.Len(t, changes, 1)
		assert.Equal(t, "orders", changes[0].(bson.M)["name"])
	})

	t.Run("NotAnArray_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.D{{Key: "orders", Value: int32(1)}}}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})

	t.Run("DocumentValueIsAValidID", func(t *testing.T) {
		// A document value is a subdocument _id, NOT a query predicate -- it must
		// be accepted (and simply match nothing here, since no such _id exists).
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.D{{Key: "a", Value: int32(1)}}}},
		}}}
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Decode(&raw))
		assert.Empty(t, raw["commits"].(bson.A))
	})

	t.Run("IDListArrayElement_Errors", func(t *testing.T) {
		// An array can never be an _id, so an array element inside the id-list
		// is invalid.
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{bson.A{int32(1)}}}},
		}}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})

	t.Run("MultiKeyEntry_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: int32(1)}, {Key: "users", Value: int32(1)}},
		}}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})
}

func TestLogIDFilterHandler_ExoticIDs(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logidx%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	c := env.Client.Database(dbName).Collection("items")

	oid := bson.NewObjectID()
	subID := bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: "x"}} // document _id

	_, err := c.InsertOne(ctx, bson.D{{Key: "_id", Value: oid}, {Key: "v", Value: 1}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "add-oid", "a <a@x.io>")
	_, err = c.InsertOne(ctx, bson.D{{Key: "_id", Value: subID}, {Key: "v", Value: 1}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "add-subdoc", "a <a@x.io>")
	_, err = c.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(7)}, {Key: "v", Value: 1}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "add-int", "a <a@x.io>")

	msg1 := func(raw bson.M) string {
		arr := raw["commits"].(bson.A)
		require.Len(t, arr, 1)
		return arr[0].(bson.M)["message"].(string)
	}

	t.Run("ObjectIDFilter", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "items", Value: oid}}}}})
		assert.Equal(t, "add-oid", msg1(raw))
	})

	t.Run("DocumentIDFilter", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{bson.D{{Key: "items", Value: subID}}}}})
		assert.Equal(t, "add-subdoc", msg1(raw))
	})

	t.Run("DocumentIDFieldOrderSignificant", func(t *testing.T) {
		// {b:"x",a:1} is a different _id from {a:1,b:"x"}; matches nothing here.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "items", Value: bson.D{{Key: "b", Value: "x"}, {Key: "a", Value: int32(1)}}}},
		}}})
		assert.Empty(t, raw["commits"].(bson.A))
	})

	t.Run("MixedIDListSugar", func(t *testing.T) {
		// array of mixed-type _ids: ObjectId + int.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "items", Value: bson.A{oid, int32(7)}}},
		}}})
		arr := raw["commits"].(bson.A)
		require.Len(t, arr, 2) // add-int and add-oid
	})
}

func TestLogMatchHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logmatch%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	orders := env.Client.Database(dbName).Collection("orders")

	// c1 add o1(pending),o2(shipped); c2 add o3(pending); c3 ship o1; c4 add user
	_, err := orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "status", Value: "pending"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "status", Value: "shipped"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c1", "a <a@x.io>")
	_, err = orders.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "status", Value: "pending"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c2", "a <a@x.io>")
	_, err = orders.UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "shipped"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c3", "a <a@x.io>")
	_, err = env.Client.Database(dbName).Collection("users").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c4", "a <a@x.io>")

	msgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("MatchTouchedPerCommit", func(t *testing.T) {
		// Touched {status:pending}: c1 (add o1 pending), c2 (add o3),
		// c3 (modify o1 pending->shipped, pre pending). c4 = users only.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "pending"}}}}}}},
		}}})
		assert.Equal(t, []string{"c3", "c2", "c1"}, msgs(raw))
	})

	t.Run("MultipleMatchOR", func(t *testing.T) {
		// pending {c3,c2,c1} OR shipped {c3(o1->shipped),c1(o2 added)} -> c3,c2,c1.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "pending"}}}},
				bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "shipped"}}}},
			}}},
		}}})
		assert.Equal(t, []string{"c3", "c2", "c1"}, msgs(raw))
	})

	t.Run("MatchMixedWithID", func(t *testing.T) {
		// pending {c3,c2,c1} OR _id 1 {c1,c3} -> c3,c2,c1.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "pending"}}}},
				int32(1),
			}}},
		}}})
		assert.Equal(t, []string{"c3", "c2", "c1"}, msgs(raw))
	})

	t.Run("UnknownOperator_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.D{{Key: "$nope", Value: int32(1)}}}},
		}}}
		require.Error(t, env.Client.Database(dbName).RunCommand(ctx, cmd).Err())
	})
}

func TestLogChangedHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logchg%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	orders := env.Client.Database(dbName).Collection("orders")

	// c1 add o1{cust 4242, pending}, o2{cust 9999, pending}
	_, err := orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "customer", Value: "4242"}, {Key: "status", Value: "pending"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "customer", Value: "9999"}, {Key: "status", Value: "pending"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c1 add", "a <a@x.io>")
	// c2: o1 status -> shipped (status changed, customer 4242)
	_, err = orders.UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "shipped"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c2 ship o1", "a <a@x.io>")
	// c3: o2 status -> shipped (status changed, customer 9999)
	_, err = orders.UpdateByID(ctx, int32(2), bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "shipped"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c3 ship o2", "a <a@x.io>")
	// c4: o1 note (status NOT changed)
	_, err = orders.UpdateByID(ctx, int32(1), bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "x"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c4 note o1", "a <a@x.io>")

	msgs := func(raw bson.M) []string {
		arr := raw["commits"].(bson.A)
		out := make([]string, len(arr))
		for i, c := range arr {
			out[i] = c.(bson.M)["message"].(string)
		}
		return out
	}

	t.Run("ChangedAnyValue", func(t *testing.T) {
		// commits where status changed: c1(add, presence), c2, c3. c4 (note only) excluded.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: bson.D{{Key: "$changed", Value: true}}}}}}}},
		}}})
		assert.Equal(t, []string{"c3 ship o2", "c2 ship o1", "c1 add"}, msgs(raw))
	})

	t.Run("ChangedAndCustomer", func(t *testing.T) {
		// status changed AND customer 4242: c2 (o1), c1 (o1 added pending, presence). Not c3 (o2/9999).
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.D{{Key: "$match", Value: bson.D{
				{Key: "status", Value: bson.D{{Key: "$changed", Value: true}}},
				{Key: "customer", Value: "4242"},
			}}}}},
		}}})
		assert.Equal(t, []string{"c2 ship o1", "c1 add"}, msgs(raw))
	})

	t.Run("NoteChangedExcludesStatusOnly", func(t *testing.T) {
		// only c4 changed note.
		raw := runLog(t, env, dbName, bson.D{{Key: "filters", Value: bson.A{
			bson.D{{Key: "orders", Value: bson.D{{Key: "$match", Value: bson.D{{Key: "note", Value: bson.D{{Key: "$changed", Value: true}}}}}}}},
		}}})
		assert.Equal(t, []string{"c4 note o1"}, msgs(raw))
	})
}
