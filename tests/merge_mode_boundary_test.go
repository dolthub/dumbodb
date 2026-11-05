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
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// txnBoundaryEdit is one writer's $set on the seed document
// {_id: "d", v: 1, a: "seed", b: "seed"}.
type txnBoundaryEdit struct {
	field string
	value any
}

// A client transaction reconciles at commitTransaction against the snapshot it
// pinned with its first write. A plain write that changes a document after
// that, and before the transaction edits the same document, leaves the
// transaction's edit and the published one with a common base, and the
// collection's merge mode alone decides whether the commit is accepted. The
// transaction holds no lock on the document until it writes it, so nothing
// else decides first.
func TestMergeMode_GovernsTheTransactionBoundary(t *testing.T) {
	shapes := []struct {
		name               string
		published, txnEdit txnBoundaryEdit
	}{
		{"convergent", txnBoundaryEdit{"v", int32(2)}, txnBoundaryEdit{"v", int32(2)}},
		{"disjoint", txnBoundaryEdit{"a", "published"}, txnBoundaryEdit{"b", "txn"}},
	}
	expectations := []struct {
		mode                     string
		onConvergent, onDisjoint bool
	}{
		{"documentTouched", true, true},
		{"fieldTouched", true, false},
		{"fieldDivergent", false, false},
		{"documentDivergent", false, true},
	}

	env := startDumboDB(t)
	ctx := context.Background()
	for _, exp := range expectations {
		for _, shape := range shapes {
			wantConflict := exp.onConvergent
			if shape.name == "disjoint" {
				wantConflict = exp.onDisjoint
			}
			t.Run(exp.mode+"/"+shape.name, func(t *testing.T) {
				dbName := fmt.Sprintf("mmtxn_%d", rand.Int64N(1_000_000))
				db := env.Client.Database(dbName)
				require.NoError(t, db.RunCommand(ctx, bson.D{
					{Key: "create", Value: "docs"},
					{Key: "mergeMode", Value: exp.mode},
				}).Err())
				docs := db.Collection("docs")
				_, err := docs.InsertMany(ctx, []any{
					bson.D{{Key: "_id", Value: "pin"}},
					bson.D{{Key: "_id", Value: "d"}, {Key: "v", Value: int32(1)}, {Key: "a", Value: "seed"}, {Key: "b", Value: "seed"}},
				})
				require.NoError(t, err)

				client := newClient(t, env)
				sess, err := client.StartSession()
				require.NoError(t, err)
				defer sess.EndSession(ctx)
				sc := mongo.NewSessionContext(ctx, sess)
				txnDocs := client.Database(dbName).Collection("docs")

				require.NoError(t, sess.StartTransaction(options.Transaction()))
				_, err = txnDocs.UpdateOne(sc, bson.D{{Key: "_id", Value: "pin"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "touched", Value: true}}}})
				require.NoError(t, err, "the transaction's first write pins its snapshot")

				setField(t, ctx, docs, shape.published)
				setField(t, sc, txnDocs, shape.txnEdit)

				err = sess.CommitTransaction(ctx)
				var stored bson.M
				require.NoError(t, docs.FindOne(ctx, bson.D{{Key: "_id", Value: "d"}}).Decode(&stored))
				if wantConflict {
					var cmdErr mongo.CommandError
					require.True(t, errors.As(err, &cmdErr),
						"mergeMode=%s must refuse a %s edit at commitTransaction, got %v", exp.mode, shape.name, err)
					require.EqualValues(t, 112, cmdErr.Code, "expected WriteConflict, got %v", err)
					require.True(t, cmdErr.HasErrorLabel("TransientTransactionError"), "the driver must be told to retry: %v", err)
					requireFields(t, stored, shape.published)
					return
				}
				require.NoError(t, err, "mergeMode=%s must accept a %s edit at commitTransaction", exp.mode, shape.name)
				requireFields(t, stored, shape.published, shape.txnEdit)
			})
		}
	}
}

func setField(t *testing.T, ctx context.Context, coll *mongo.Collection, edit txnBoundaryEdit) {
	t.Helper()
	res, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: "d"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: edit.field, Value: edit.value}}}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.MatchedCount)
}

// requireFields checks stored is the seed document with edits applied.
func requireFields(t *testing.T, stored bson.M, edits ...txnBoundaryEdit) {
	t.Helper()
	want := bson.M{"v": int32(1), "a": "seed", "b": "seed"}
	for _, e := range edits {
		want[e.field] = e.value
	}
	for k, v := range want {
		require.EqualValues(t, v, stored[k], "field %q", k)
	}
}
