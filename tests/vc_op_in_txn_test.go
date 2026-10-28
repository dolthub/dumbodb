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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A version-control operation run inside a client transaction counts the
// transaction's own unpublished writes as uncommitted changes, just as it
// counts published ones. Run in a transaction that has written, it is refused
// and the transaction's writes survive to its commit; run in one that has not,
// it goes ahead.
func TestVCOpsInATransactionCountItsUnpublishedWrites(t *testing.T) {
	type fixture struct {
		dbName, featureCommit, mainCommit, baseCommit string
	}
	ops := map[string]func(f fixture) bson.D{
		"merge": func(fixture) bson.D {
			return bson.D{{Key: "dumboMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}}
		},
		"cherry-pick": func(f fixture) bson.D {
			return bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: f.featureCommit}}
		},
		"revert": func(f fixture) bson.D {
			return bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "commit", Value: f.mainCommit}}
		},
		"rebase": func(fixture) bson.D {
			return bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "feature"}}
		},
		"soft reset": func(f fixture) bson.D {
			return bson.D{{Key: "dumboReset", Value: int32(1)}, {Key: "to", Value: f.baseCommit}}
		},
		"hard reset": func(f fixture) bson.D {
			return bson.D{{Key: "dumboReset", Value: int32(1)}, {Key: "to", Value: f.baseCommit}, {Key: "hard", Value: true}}
		},
	}
	env := startDumboDB(t)
	ctx := context.Background()
	commit := func(db, message string) string {
		var res bson.M
		require.NoError(t, env.Client.Database(db).RunCommand(ctx, bson.D{
			{Key: "dumboCommit", Value: int32(1)}, {Key: "message", Value: message}, {Key: "author", Value: "a <a@example.com>"},
		}).Decode(&res))
		return res["commitId"].(string)
	}
	setup := func(t *testing.T) fixture {
		f := fixture{dbName: fmt.Sprintf("vcintxn%d", rand.Int64N(1_000_000))}
		main := env.Client.Database(f.dbName)
		_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: "base"}})
		require.NoError(t, err)
		f.baseCommit = commit(f.dbName, "base")
		require.NoError(t, env.Client.Database(f.dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
		}).Err())
		_, err = env.Client.Database(f.dbName+"@feature").Collection("side").InsertOne(ctx, bson.D{{Key: "_id", Value: "feat"}})
		require.NoError(t, err)
		f.featureCommit = commit(f.dbName+"@feature", "feat")
		_, err = main.Collection("side").InsertOne(ctx, bson.D{{Key: "_id", Value: "main"}})
		require.NoError(t, err)
		f.mainCommit = commit(f.dbName, "main")
		return f
	}

	for name, op := range ops {
		t.Run(name+"/with unpublished writes", func(t *testing.T) {
			f := setup(t)
			client := siClient(t, env)
			sess, err := client.StartSession()
			require.NoError(t, err)
			defer sess.EndSession(ctx)
			sc := mongo.NewSessionContext(ctx, sess)
			require.NoError(t, sess.StartTransaction(options.Transaction()))
			_, err = client.Database(f.dbName).Collection("items").InsertOne(sc, bson.D{{Key: "_id", Value: "in-txn"}})
			require.NoError(t, err)

			err = client.Database(f.dbName).RunCommand(sc, op(f)).Err()
			require.ErrorContains(t, err, "uncommitted", "the operation must count the transaction's writes")
			require.EqualValues(t, 117, mongoCommandCode(err), "a refusal over uncommitted changes is ConflictingOperationInProgress")
			require.NoError(t, sess.CommitTransaction(ctx))

			n, err := env.Client.Database(f.dbName).Collection("items").CountDocuments(ctx, bson.D{{Key: "_id", Value: "in-txn"}})
			require.NoError(t, err)
			require.EqualValues(t, 1, n, "the transaction's acknowledged write was lost")
		})
		t.Run(name+"/without writes", func(t *testing.T) {
			f := setup(t)
			client := siClient(t, env)
			sess, err := client.StartSession()
			require.NoError(t, err)
			defer sess.EndSession(ctx)
			sc := mongo.NewSessionContext(ctx, sess)
			require.NoError(t, sess.StartTransaction(options.Transaction()))
			_, err = client.Database(f.dbName).Collection("items").Find(sc, bson.D{})
			require.NoError(t, err)
			require.NoError(t, client.Database(f.dbName).RunCommand(sc, op(f)).Err(), "a transaction that has written nothing does not stop the operation")
			require.NoError(t, sess.CommitTransaction(ctx))
		})
	}
}
