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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestCollection_Distinct tests the distinct command for nested fields, array fields,
// and query-filtered distinct.
func TestCollection_Distinct(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "alice"), e("address", d(e("city", "NYC"), e("zip", "10001"))), e("tags", bson.A{"go", "db"})),
		d(e("_id", "bob"), e("address", d(e("city", "LA"), e("zip", "90001"))), e("tags", bson.A{"db", "sql"})),
		d(e("_id", "charlie"), e("address", d(e("city", "NYC"), e("zip", "10002"))), e("tags", bson.A{"go", "nosql"})),
		d(e("_id", "dana"), e("address", d(e("city", "SF"), e("zip", "94105"))), e("tags", bson.A{"go"})),
		d(e("_id", "eve"), e("tags", bson.A{"sql", "nosql"})),
	)

	ctx := context.Background()

	t.Run("NestedFieldPath", func(t *testing.T) {
		t.Parallel()
		// Distinct on a nested field path should traverse nested documents.
		result, err := coll.Distinct(ctx, "address.city", bson.D{})
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool {
			return result[i].(string) < result[j].(string)
		})
		assert.Equal(t, []interface{}{"LA", "NYC", "SF"}, result)
	})

	t.Run("ArrayField", func(t *testing.T) {
		t.Parallel()
		// Distinct on an array field should return individual array elements, not arrays.
		result, err := coll.Distinct(ctx, "tags", bson.D{})
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool {
			return result[i].(string) < result[j].(string)
		})
		assert.Equal(t, []interface{}{"db", "go", "nosql", "sql"}, result)
	})

	t.Run("WithQueryFilter", func(t *testing.T) {
		t.Parallel()
		// Distinct with a query filter should only consider matching documents.
		filter := d(e("address.city", "NYC"))
		result, err := coll.Distinct(ctx, "tags", filter)
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool {
			return result[i].(string) < result[j].(string)
		})
		// Only alice (NYC) and charlie (NYC) match; their tags are: go, db, nosql.
		assert.Equal(t, []interface{}{"db", "go", "nosql"}, result)
	})

	t.Run("NestedFieldMissing", func(t *testing.T) {
		t.Parallel()
		// Documents without the nested field should be ignored.
		result, err := coll.Distinct(ctx, "address.zip", bson.D{})
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool {
			return result[i].(string) < result[j].(string)
		})
		// eve has no address field, so only 4 values from alice/bob/charlie/dana.
		assert.Equal(t, []interface{}{"10001", "10002", "90001", "94105"}, result)
	})
}

// TestCollection_EstimatedDocumentCount tests that EstimatedDocumentCount returns
// the correct document count using collection metadata.
func TestCollection_EstimatedDocumentCount(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	t.Run("EmptyCollection", func(t *testing.T) {
		t.Parallel()
		emptyColl := env.collection(t)
		count, err := emptyColl.EstimatedDocumentCount(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("AfterInsert", func(t *testing.T) {
		t.Parallel()
		localColl := env.collection(t)
		insertDocs(t, localColl,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		)
		count, err := localColl.EstimatedDocumentCount(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(3), count)
	})

	t.Run("CountMatchesActual", func(t *testing.T) {
		t.Parallel()
		insertDocs(t, coll,
			d(e("_id", "x")),
			d(e("_id", "y")),
			d(e("_id", "z")),
			d(e("_id", "w")),
			d(e("_id", "v")),
		)

		estimated, err := coll.EstimatedDocumentCount(ctx)
		require.NoError(t, err)

		exact, err := coll.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)

		// EstimatedDocumentCount should match the exact count for our small test collections.
		assert.Equal(t, exact, estimated)
	})
}
