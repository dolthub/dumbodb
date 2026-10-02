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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// An open cursor must keep returning the documents it was opened over even
// when dumboGC runs between batches and nothing else references them.
func TestGC_IdleCursorSurvivesGC(t *testing.T) {
	for _, mode := range []string{"default", "full"} {
		t.Run(mode, func(t *testing.T) {
			env := startDumboDB(t)
			ctx := context.Background()
			db := env.Client.Database(fmt.Sprintf("testdb_gc_cursor_%d", rand.Int64()))
			t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck
			coll := db.Collection("c")

			const total = 3000
			padding := strings.Repeat("x", 200)
			docs := make([]any, total)
			for i := range docs {
				docs[i] = bson.D{{Key: "_id", Value: int32(i)}, {Key: "pad", Value: padding}}
			}
			_, err := coll.InsertMany(ctx, docs)
			require.NoError(t, err)

			cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(10))
			require.NoError(t, err)
			t.Cleanup(func() { cursor.Close(context.Background()) }) //nolint:errcheck
			seen := 0
			for i := 0; i < 10 && cursor.Next(ctx); i++ {
				seen++
			}

			other, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://127.0.0.1:%d/", env.Port)))
			require.NoError(t, err)
			t.Cleanup(func() { other.Disconnect(context.Background()) }) //nolint:errcheck
			_, err = other.Database(db.Name()).Collection("c").DeleteMany(ctx, bson.D{})
			require.NoError(t, err)
			require.NoError(t, other.Database(db.Name()).RunCommand(ctx, bson.D{{Key: "dumboGC", Value: 1}, {Key: "mode", Value: mode}}).Err())

			for cursor.Next(ctx) {
				seen++
			}
			require.NoError(t, cursor.Err())
			require.Equal(t, total, seen)
		})
	}
}
