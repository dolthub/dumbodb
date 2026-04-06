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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestCollection_CreateCollation verifies that a collection can be created with
// a collation option without returning an error (do-ch3o).
// Docudolt accepts collation at creation time but does not enforce it.
func TestCollection_CreateCollation(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()
	db := env.client.Database("testdb")

	collName := fmt.Sprintf("col_collation_%d", rand.Int64())
	t.Cleanup(func() {
		db.Collection(collName).Drop(context.Background()) //nolint:errcheck
	})

	createOpts := options.CreateCollection().SetCollation(&options.Collation{
		Locale:   "en",
		Strength: 2,
	})
	err := db.CreateCollection(ctx, collName, createOpts)
	require.NoError(t, err, "creating a collection with collation must not return an error")
}

// TestCollection_ListCollectionsIdIndex verifies that listCollections returns an
// idIndex field for regular (non-view) collections (do-ch3o).
func TestCollection_ListCollectionsIdIndex(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a document so the collection is guaranteed to exist.
	_, err := coll.InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
	require.NoError(t, err)

	cursor, err := env.client.Database("testdb").ListCollections(ctx, bson.D{})
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.Raw
	require.NoError(t, cursor.All(ctx, &results))

	found := false
	for _, r := range results {
		nameVal, ok := r.Lookup("name").StringValueOK()
		if !ok || nameVal != coll.Name() {
			continue
		}
		found = true

		// idIndex must be present for a regular collection.
		idIndex, ok := r.Lookup("idIndex").DocumentOK()
		require.True(t, ok, "idIndex must be a document in listCollections output")

		// idIndex must have v=2, key={_id:1}, name="_id_".
		v := idIndex.Lookup("v").Int32()
		require.Equal(t, int32(2), v, "idIndex.v must be 2")

		keyDoc, ok := idIndex.Lookup("key").DocumentOK()
		require.True(t, ok, "idIndex.key must be a document")
		idField := keyDoc.Lookup("_id").Int32()
		require.Equal(t, int32(1), idField, "idIndex.key._id must be 1")

		idxName, ok := idIndex.Lookup("name").StringValueOK()
		require.True(t, ok, "idIndex.name must be a string")
		require.Equal(t, "_id_", idxName, "idIndex.name must be '_id_'")
	}

	require.True(t, found, "collection %q not found in listCollections output", coll.Name())
}

// TestCursor_CollationCaseInsensitive verifies that find with a case-insensitive
// collation (strength ≤ 2) returns both exact-case and differently-cased documents.
// Parity test for do-7133: collation case-insensitive find returns empty results. (DocudoltFull)
func TestCursor_CollationCaseInsensitive(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "Alice"}},
		bson.D{{Key: "name", Value: "alice"}},
	)

	// Find with case-insensitive collation — must return both variants.
	findOpts := options.Find().SetCollation(&options.Collation{
		Locale:   "en",
		Strength: 2,
	})
	cur, err := coll.Find(ctx, bson.D{{Key: "name", Value: "alice"}}, findOpts)
	require.NoError(t, err, "find with collation must not return an error")
	defer cur.Close(ctx)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	// Both "alice" and "Alice" must be returned with case-insensitive collation.
	require.Len(t, results, 2, "case-insensitive find must return both case variants")
}

// TestCursor_CollationSort verifies that a find command with both a collation
// option and a sort directive is accepted without returning an error (do-ch3o).
// Docudolt accepts the collation option but does not enforce locale-aware sort
// order — all documents are still returned.
func TestCursor_CollationSort(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "Charlie"}},
		bson.D{{Key: "name", Value: "alice"}},
		bson.D{{Key: "name", Value: "Bob"}},
	)

	// Find + sort with collation — must not return an error.
	findOpts := options.Find().
		SetCollation(&options.Collation{Locale: "en", Strength: 2}).
		SetSort(bson.D{{Key: "name", Value: 1}})
	cur, err := coll.Find(ctx, bson.D{}, findOpts)
	require.NoError(t, err, "find with collation+sort must not return an error")
	defer cur.Close(ctx)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Len(t, results, 3, "all documents must be returned")
}

// TestCursor_NoCursorTimeout verifies that the noCursorTimeout find option is
// accepted without returning an error (do-ch3o).
// Docudolt ignores this option — cursor timeout behaviour is unchanged.
func TestCursor_NoCursorTimeout(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "x", Value: 1}},
		bson.D{{Key: "x", Value: 2}},
	)

	// Find with NoCursorTimeout — must not return an error.
	findOpts := options.Find().SetNoCursorTimeout(true)
	cur, err := coll.Find(ctx, bson.D{}, findOpts)
	require.NoError(t, err, "find with NoCursorTimeout must not return an error")
	defer cur.Close(ctx)

	var results []bson.D
	require.NoError(t, cur.All(ctx, &results))
	require.Len(t, results, 2)
}
