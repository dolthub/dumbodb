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
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Before the fix $range had no output limit, so one small aggregate could
// allocate billions of elements and the out-of-memory kill took down the
// whole server.
func TestExpr_rangeOverMemoryLimitIsRejected(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)
	ctx := context.Background()
	insertDocs(t, coll, d(e("_id", "a")))

	_, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{{"r", bson.D{{"$range", bson.A{int32(0), int32(2_000_000_000)}}}}}}},
	})
	var cmdErr mongo.CommandError
	require.True(t, errors.As(err, &cmdErr), "expected a command error, got %v", err)
	require.EqualValues(t, 146, cmdErr.Code, "%v", err)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{{"r", bson.D{{"$range", bson.A{int32(0), int32(3)}}}}}}},
	})
	require.NoError(t, err, "the server must still be serving")
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Equal(t, bson.A{int32(0), int32(1), int32(2)}, dmap(results[0])["r"])
}
