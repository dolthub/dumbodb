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

// A getMore on a cursor whose collection or database another client dropped
// fails with 175 QueryPlanKilled, as in mongod, not 43 CursorNotFound.
func TestGetMoreAfterDropIsQueryPlanKilled(t *testing.T) {
	for _, drop := range []string{"database", "collection"} {
		t.Run(drop, func(t *testing.T) {
			env := startDumboDB(t)
			ctx := context.Background()
			db := env.Client.Database(fmt.Sprintf("testdb_drop_cursor_%d", rand.Int64()))
			t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck
			coll := db.Collection("c")

			docs := make([]any, 10)
			for i := range docs {
				docs[i] = bson.D{{Key: "_id", Value: int32(i)}}
			}
			_, err := coll.InsertMany(ctx, docs)
			require.NoError(t, err)

			cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(2))
			require.NoError(t, err)
			t.Cleanup(func() { cursor.Close(context.Background()) }) //nolint:errcheck
			require.True(t, cursor.Next(ctx))
			require.True(t, cursor.Next(ctx))

			other, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://127.0.0.1:%d/", env.Port)))
			require.NoError(t, err)
			t.Cleanup(func() { other.Disconnect(context.Background()) }) //nolint:errcheck
			otherDB := other.Database(db.Name())
			if drop == "database" {
				require.NoError(t, otherDB.Drop(ctx))
			} else {
				require.NoError(t, otherDB.Collection("c").Drop(ctx))
			}

			require.False(t, cursor.Next(ctx))
			var cmdErr mongo.CommandError
			require.True(t, errors.As(cursor.Err(), &cmdErr), "got %v", cursor.Err())
			require.Equal(t, int32(175), cmdErr.Code, "got %v", cmdErr)
			require.Equal(t, "QueryPlanKilled", cmdErr.Name)
		})
	}
}
