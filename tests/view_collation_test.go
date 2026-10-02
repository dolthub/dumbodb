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

func TestView_CollationAppliesToReads(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("testdb_view_collation_%d", rand.Int64()))
	t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck

	_, err := db.Collection("base").InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "u", Value: "Alice"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "u", Value: "alice"}},
		bson.D{{Key: "_id", Value: 3}, {Key: "u", Value: "BOB"}},
	})
	require.NoError(t, err)

	enS2 := &options.Collation{Locale: "en", Strength: 2}
	require.NoError(t, db.CreateView(ctx, "v", "base", bson.A{}, options.CreateView().SetCollation(enS2)))
	view := db.Collection("v")

	specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: "v"}})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	strength, err := specs[0].Options.LookupErr("collation", "strength")
	require.NoError(t, err, "listCollections must report the view's collation")
	require.EqualValues(t, 2, strength.AsInt64())

	n, err := view.CountDocuments(ctx, bson.D{{Key: "u", Value: "alice"}})
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "a read on the view uses the view's collation")

	values := view.Distinct(ctx, "u", bson.D{})
	require.NoError(t, values.Err())
	var distinct []any
	require.NoError(t, values.Decode(&distinct))
	require.Len(t, distinct, 2, "distinct de-duplicates under the view's collation")

	_, err = view.Find(ctx, bson.D{}, options.Find().SetCollation(&options.Collation{Locale: "fr"}))
	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr)
	require.EqualValues(t, 167, cmdErr.Code, "a differing collation cannot override the view's")
}

func TestDistinct_DeduplicatesUnderCollectionCollation(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("testdb_distinct_collation_%d", rand.Int64()))
	t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck

	require.NoError(t, db.CreateCollection(ctx, "en2", options.CreateCollection().SetCollation(&options.Collation{Locale: "en", Strength: 2})))
	coll := db.Collection("en2")
	_, err := coll.InsertMany(ctx, []any{
		bson.D{{Key: "u", Value: "Alice"}},
		bson.D{{Key: "u", Value: "alice"}},
		bson.D{{Key: "u", Value: "BOB"}},
	})
	require.NoError(t, err)

	values := coll.Distinct(ctx, "u", bson.D{})
	require.NoError(t, values.Err())
	var distinct []any
	require.NoError(t, values.Decode(&distinct))
	require.Len(t, distinct, 2)
}
