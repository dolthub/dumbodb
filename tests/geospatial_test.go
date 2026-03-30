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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// TestGeo_near_InvalidPointLongitude verifies that $near with $geometry and
// longitude > 180 returns MongoDB-compatible error. Parity test for do-bpmo.
func TestGeo_near_InvalidPointLongitude(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("loc", d(e("$near", d(
			e("$geometry", d(
				e("type", "Point"),
				e("coordinates", bson.A{float64(200), float64(0)}), // lon 200 > 180
			)),
		))))),
	)
	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 2, cmdErr.Code, "expected BadValue (2), got code %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, cmdErr.Message, "Longitude/latitude is out of bounds", "unexpected error message: %s", cmdErr.Message)
}

// TestGeo_near_InvalidPointLatitude verifies that $near with $geometry and
// latitude > 90 returns MongoDB-compatible error. Parity test for do-bpmo.
func TestGeo_near_InvalidPointLatitude(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("loc", d(e("$near", d(
			e("$geometry", d(
				e("type", "Point"),
				e("coordinates", bson.A{float64(0), float64(100)}), // lat 100 > 90
			)),
		))))),
	)
	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 2, cmdErr.Code, "expected BadValue (2), got code %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, cmdErr.Message, "Longitude/latitude is out of bounds", "unexpected error message: %s", cmdErr.Message)
}

// TestGeo_nearSphere_InvalidPoint verifies that $nearSphere with $geometry and
// an out-of-range longitude returns MongoDB-compatible error. Parity test for do-bpmo.
func TestGeo_nearSphere_InvalidPoint(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("loc", d(e("$nearSphere", d(
			e("$geometry", d(
				e("type", "Point"),
				e("coordinates", bson.A{float64(200), float64(0)}), // lon 200 > 180
			)),
		))))),
	)
	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 2, cmdErr.Code, "expected BadValue (2), got code %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, cmdErr.Message, "Longitude/latitude is out of bounds", "unexpected error message: %s", cmdErr.Message)
}

// TestGeo_geoNear_InvalidPoint verifies that the $geoNear aggregation stage
// with an out-of-range coordinate returns MongoDB-compatible error
// "invalid argument in geo near query: type". Parity test for do-bpmo.
func TestGeo_geoNear_InvalidPoint(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Aggregate(ctx, bson.A{
		d(e("$geoNear", d(
			e("near", d(
				e("type", "Point"),
				e("coordinates", bson.A{float64(200), float64(0)}), // lon 200 > 180
			)),
			e("distanceField", "dist"),
			e("spherical", true),
		))),
	})
	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 2, cmdErr.Code, "expected BadValue (2), got code %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, cmdErr.Message, "invalid argument in geo near query: type", "unexpected error message: %s", cmdErr.Message)
}

// TestGeo_Legacy_NearSphere_2d verifies that $nearSphere with a legacy 2d index
// correctly handles $maxDistance given in radians.  The query is centred on London
// with a ~15-degree (~1670 km) radius and must return exactly two of the three
// documents: London itself and Paris, but not Moscow.
//
// Before the do-twgm fix, dongo compared haversine metres directly against the
// raw radian value (≈ 0.26), so only the document at the query point (distance
// ≈ 0 m) passed the filter; all other documents were incorrectly excluded.
// Regression for do-twgm / do-pyxs.
func TestGeo_Legacy_NearSphere_2d(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Create a legacy 2d index on the location field.
	model := mongo.IndexModel{Keys: bson.D{{Key: "loc", Value: "2d"}}}
	_, err := coll.Indexes().CreateOne(context.Background(), model)
	require.NoError(t, err)

	// Three cities stored as legacy [lon, lat] arrays.
	// London [-0.12, 51.50] is the query origin (distance 0).
	// Paris  [ 2.35, 48.85] is ~340 km from London — inside the 15° radius.
	// Moscow [37.62, 55.75] is ~2500 km from London — outside the 15° radius.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", bson.A{float64(-0.12), float64(51.50)})), // London
		d(e("_id", int32(2)), e("loc", bson.A{float64(2.35), float64(48.85)})), // Paris
		d(e("_id", int32(3)), e("loc", bson.A{float64(37.62), float64(55.75)})), // Moscow
	)

	ctx := context.Background()

	// $maxDistance in radians: 15° ≈ 0.2618 rad ≈ 1670 km.
	// Dongo must convert radians → metres before applying the haversine filter.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(
			e("$nearSphere", bson.A{float64(-0.12), float64(51.50)}),
			e("$maxDistance", float64(0.2618)),
		))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2, "expected London and Paris within 15° radius, got %d results", len(results))
}
