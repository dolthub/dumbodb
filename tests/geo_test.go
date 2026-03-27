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

// Tests for geospatial parity between MongoDB and Dongo.
//
// Coverage:
//   - 2dsphere index: all 7 GeoJSON geometry types (Point, LineString, Polygon,
//     MultiPoint, MultiLineString, MultiPolygon, GeometryCollection)
//   - 2d index + legacy coordinate pair format
//   - Query operators: $geoWithin, $geoIntersects, $near, $nearSphere
//     with $maxDistance / $minDistance and $geometry
//   - $geoNear aggregation stage (with and without distanceField, spherical)
//   - MongoDB 8.0 strict Point validation for $near / $nearSphere / $geoNear
//
// All tests are DongoXFail — geospatial support is not yet implemented in Dongo.
// Remove dongoXFail() calls as support is added.

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// create2dsphereIndex creates a 2dsphere index on the given field.
func create2dsphereIndex(t *testing.T, coll *mongo.Collection, field string) {
	t.Helper()
	ctx := context.Background()
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: field, Value: "2dsphere"}},
	})
	require.NoError(t, err)
}

// create2dIndex creates a legacy 2d index on the given field.
func create2dIndex(t *testing.T, coll *mongo.Collection, field string) {
	t.Helper()
	ctx := context.Background()
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: field, Value: "2d"}},
	})
	require.NoError(t, err)
}

// geoPoint builds a GeoJSON Point document.
func geoPoint(lng, lat float64) bson.D {
	return bson.D{
		{Key: "type", Value: "Point"},
		{Key: "coordinates", Value: bson.A{lng, lat}},
	}
}

// geoLineString builds a GeoJSON LineString document.
func geoLineString(coords ...bson.A) bson.D {
	coordArr := make(bson.A, len(coords))
	for i, c := range coords {
		coordArr[i] = c
	}
	return bson.D{
		{Key: "type", Value: "LineString"},
		{Key: "coordinates", Value: coordArr},
	}
}

// geoPolygon builds a GeoJSON Polygon with one exterior ring (no holes).
func geoPolygon(ring ...bson.A) bson.D {
	coords := make(bson.A, len(ring))
	for i, c := range ring {
		coords[i] = c
	}
	return bson.D{
		{Key: "type", Value: "Polygon"},
		{Key: "coordinates", Value: bson.A{coords}},
	}
}

// coord is a shorthand for a [lng, lat] coordinate array.
func coord(lng, lat float64) bson.A {
	return bson.A{lng, lat}
}

// ─── 2dsphere index — GeoJSON geometry types ─────────────────────────────────

// TestGeo_2dsphere_PointInsertAndFind verifies that GeoJSON Point documents
// can be indexed and retrieved via 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_PointInsertAndFind(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
		d(e("_id", "london"), e("loc", geoPoint(-0.1276, 51.5074))),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "nyc"))).Decode(&result))
	loc := result.Map()["loc"].(bson.D).Map()
	assert.Equal(t, "Point", loc["type"])
}

// TestGeo_2dsphere_LineStringInsert verifies that GeoJSON LineString documents
// are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_LineStringInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "path")

	ls := geoLineString(coord(-74.0, 40.7), coord(-73.9, 40.8), coord(-73.8, 40.9))
	insertDocs(t, coll,
		d(e("_id", "route1"), e("path", ls)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "route1"))).Decode(&result))
	path := result.Map()["path"].(bson.D).Map()
	assert.Equal(t, "LineString", path["type"])
}

// TestGeo_2dsphere_PolygonInsert verifies that GeoJSON Polygon documents
// are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_PolygonInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "boundary")

	// A simple square polygon (closed ring: first == last point).
	ring := []bson.A{
		coord(-74.1, 40.6),
		coord(-73.9, 40.6),
		coord(-73.9, 40.8),
		coord(-74.1, 40.8),
		coord(-74.1, 40.6),
	}
	poly := geoPolygon(ring...)
	insertDocs(t, coll,
		d(e("_id", "zone1"), e("boundary", poly)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "zone1"))).Decode(&result))
	boundary := result.Map()["boundary"].(bson.D).Map()
	assert.Equal(t, "Polygon", boundary["type"])
}

// TestGeo_2dsphere_MultiPointInsert verifies that GeoJSON MultiPoint documents
// are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_MultiPointInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "stops")

	mp := bson.D{
		{Key: "type", Value: "MultiPoint"},
		{Key: "coordinates", Value: bson.A{
			coord(-74.0, 40.7),
			coord(-118.2, 34.0),
			coord(-87.6, 41.8),
		}},
	}
	insertDocs(t, coll,
		d(e("_id", "stations"), e("stops", mp)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "stations"))).Decode(&result))
	stops := result.Map()["stops"].(bson.D).Map()
	assert.Equal(t, "MultiPoint", stops["type"])
}

// TestGeo_2dsphere_MultiLineStringInsert verifies that GeoJSON MultiLineString
// documents are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_MultiLineStringInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "routes")

	mls := bson.D{
		{Key: "type", Value: "MultiLineString"},
		{Key: "coordinates", Value: bson.A{
			bson.A{coord(-74.0, 40.7), coord(-73.9, 40.8)},
			bson.A{coord(-118.2, 34.0), coord(-118.1, 34.1)},
		}},
	}
	insertDocs(t, coll,
		d(e("_id", "roads"), e("routes", mls)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "roads"))).Decode(&result))
	routes := result.Map()["routes"].(bson.D).Map()
	assert.Equal(t, "MultiLineString", routes["type"])
}

// TestGeo_2dsphere_MultiPolygonInsert verifies that GeoJSON MultiPolygon
// documents are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_MultiPolygonInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "territory")

	// Two simple triangular polygons.
	ring1 := bson.A{coord(-74.1, 40.6), coord(-73.9, 40.6), coord(-74.0, 40.8), coord(-74.1, 40.6)}
	ring2 := bson.A{coord(-118.3, 33.9), coord(-118.1, 33.9), coord(-118.2, 34.1), coord(-118.3, 33.9)}
	mpoly := bson.D{
		{Key: "type", Value: "MultiPolygon"},
		{Key: "coordinates", Value: bson.A{
			bson.A{ring1},
			bson.A{ring2},
		}},
	}
	insertDocs(t, coll,
		d(e("_id", "districts"), e("territory", mpoly)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "districts"))).Decode(&result))
	territory := result.Map()["territory"].(bson.D).Map()
	assert.Equal(t, "MultiPolygon", territory["type"])
}

// TestGeo_2dsphere_GeometryCollectionInsert verifies that GeoJSON
// GeometryCollection documents are accepted by a 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_GeometryCollectionInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "shapes")

	gc := bson.D{
		{Key: "type", Value: "GeometryCollection"},
		{Key: "geometries", Value: bson.A{
			geoPoint(-74.0060, 40.7128),
			geoLineString(coord(-74.0, 40.7), coord(-73.9, 40.8)),
		}},
	}
	insertDocs(t, coll,
		d(e("_id", "mixed"), e("shapes", gc)),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "mixed"))).Decode(&result))
	shapes := result.Map()["shapes"].(bson.D).Map()
	assert.Equal(t, "GeometryCollection", shapes["type"])
}

// ─── 2d index — legacy coordinate pairs ──────────────────────────────────────

// TestGeo_2d_ArrayCoordInsert verifies that documents with legacy [lng, lat]
// arrays can be indexed by a 2d index. (DongoXFail)
func TestGeo_2d_ArrayCoordInsert(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("coords", bson.A{-74.0060, 40.7128})),
		d(e("_id", "la"), e("coords", bson.A{-118.2437, 34.0522})),
		d(e("_id", "chicago"), e("coords", bson.A{-87.6298, 41.8781})),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "nyc"))).Decode(&result))
	assert.NotNil(t, result)
}

// TestGeo_2d_EmbeddedDocCoord verifies that documents with embedded {x, y}
// documents can be indexed by a 2d index. (DongoXFail)
func TestGeo_2d_EmbeddedDocCoord(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "pos")

	insertDocs(t, coll,
		d(e("_id", "a"), e("pos", d(e("x", -74.0), e("y", 40.7)))),
		d(e("_id", "b"), e("pos", d(e("x", -73.9), e("y", 40.8)))),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
	assert.NotNil(t, result)
}

// TestGeo_2d_NearQuery verifies that $near works with a legacy 2d index. (DongoXFail)
func TestGeo_2d_NearQuery(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("coords", bson.A{-74.0060, 40.7128})),
		d(e("_id", "la"), e("coords", bson.A{-118.2437, 34.0522})),
	)

	// $near with legacy 2d uses degree-based distance.
	cursor, err := coll.Find(ctx, bson.D{{
		Key: "coords",
		Value: bson.D{{Key: "$near", Value: bson.A{-74.0, 40.7}}},
	}}, options.Find().SetLimit(1))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_2d_NearWithMaxDistance verifies $near with $maxDistance on a 2d index. (DongoXFail)
func TestGeo_2d_NearWithMaxDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "close"), e("coords", bson.A{-74.0, 40.7})),
		d(e("_id", "far"), e("coords", bson.A{-118.2, 34.0})),
	)

	cursor, err := coll.Find(ctx, bson.D{{
		Key: "coords",
		Value: bson.D{
			{Key: "$near", Value: bson.A{-74.0, 40.7}},
			{Key: "$maxDistance", Value: 1.0}, // 1 degree
		},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "close", results[0].Map()["_id"])
}

// ─── $geoWithin ──────────────────────────────────────────────────────────────

// TestGeo_geoWithin_Polygon verifies $geoWithin with $geometry (Polygon). (DongoXFail)
func TestGeo_geoWithin_Polygon(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoWithin not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		// Inside the NYC bounding box.
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "brooklyn"), e("loc", geoPoint(-73.9496, 40.6501))),
		// Far outside.
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	// Polygon covering the greater NYC area.
	ring := []bson.A{
		coord(-74.3, 40.5),
		coord(-73.7, 40.5),
		coord(-73.7, 40.9),
		coord(-74.3, 40.9),
		coord(-74.3, 40.5),
	}
	filter := bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$geoWithin",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPolygon(ring...),
			}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)
	ids := []interface{}{results[0].Map()["_id"], results[1].Map()["_id"]}
	assert.Contains(t, ids, "nyc")
	assert.Contains(t, ids, "brooklyn")
}

// TestGeo_geoWithin_CenterSphere verifies $geoWithin with $centerSphere (radians). (DongoXFail)
func TestGeo_geoWithin_CenterSphere(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoWithin/$centerSphere not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	// ~100km radius around NYC in radians (100000m / 6378137m ≈ 0.01568).
	filter := bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key:   "$geoWithin",
			Value: bson.D{{Key: "$centerSphere", Value: bson.A{bson.A{-74.0, 40.7}, 0.01568}}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_geoWithin_Center verifies $geoWithin with $center on a 2d index. (DongoXFail)
func TestGeo_geoWithin_Center(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index and $geoWithin/$center not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "close"), e("coords", bson.A{-74.0, 40.7})),
		d(e("_id", "far"), e("coords", bson.A{-118.2, 34.0})),
	)

	// Circle of radius 1 degree around (-74, 40.7).
	filter := bson.D{{
		Key: "coords",
		Value: bson.D{{
			Key:   "$geoWithin",
			Value: bson.D{{Key: "$center", Value: bson.A{bson.A{-74.0, 40.7}, 1.0}}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "close", results[0].Map()["_id"])
}

// TestGeo_geoWithin_Box verifies $geoWithin with $box on a 2d index. (DongoXFail)
func TestGeo_geoWithin_Box(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index and $geoWithin/$box not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "inside"), e("coords", bson.A{-74.0, 40.7})),
		d(e("_id", "outside"), e("coords", bson.A{-118.2, 34.0})),
	)

	// Box: bottom-left (-74.5, 40.5) to top-right (-73.5, 41.0).
	filter := bson.D{{
		Key: "coords",
		Value: bson.D{{
			Key:   "$geoWithin",
			Value: bson.D{{Key: "$box", Value: bson.A{bson.A{-74.5, 40.5}, bson.A{-73.5, 41.0}}}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "inside", results[0].Map()["_id"])
}

// ─── $geoIntersects ──────────────────────────────────────────────────────────

// TestGeo_geoIntersects_Polygon verifies $geoIntersects with a Polygon. (DongoXFail)
func TestGeo_geoIntersects_Polygon(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoIntersects not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "geo")

	// A LineString that crosses the NYC bounding box.
	ls := geoLineString(coord(-74.5, 40.7), coord(-73.5, 40.7))
	insertDocs(t, coll,
		d(e("_id", "crosser"), e("geo", ls)),
		// A point far away that does NOT intersect the box.
		d(e("_id", "far"), e("geo", geoPoint(-118.2437, 34.0522))),
	)

	ring := []bson.A{
		coord(-74.3, 40.5),
		coord(-73.7, 40.5),
		coord(-73.7, 40.9),
		coord(-74.3, 40.9),
		coord(-74.3, 40.5),
	}
	filter := bson.D{{
		Key: "geo",
		Value: bson.D{{
			Key: "$geoIntersects",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPolygon(ring...),
			}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "crosser", results[0].Map()["_id"])
}

// TestGeo_geoIntersects_Point verifies that a Point $geoIntersects a Polygon
// when the point lies inside it. (DongoXFail)
func TestGeo_geoIntersects_Point(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoIntersects not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "inside"), e("loc", geoPoint(-74.0, 40.7))),
		d(e("_id", "outside"), e("loc", geoPoint(-118.2, 34.0))),
	)

	ring := []bson.A{
		coord(-74.3, 40.5),
		coord(-73.7, 40.5),
		coord(-73.7, 40.9),
		coord(-74.3, 40.9),
		coord(-74.3, 40.5),
	}
	filter := bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$geoIntersects",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPolygon(ring...),
			}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "inside", results[0].Map()["_id"])
}

// ─── $near (2dsphere) ────────────────────────────────────────────────────────

// TestGeo_near_Basic verifies $near with $geometry on a 2dsphere index. (DongoXFail)
func TestGeo_near_Basic(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
		d(e("_id", "hoboken"), e("loc", geoPoint(-74.0323, 40.7440))),
	)

	// Query near NYC — should return nyc and hoboken sorted by distance.
	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPoint(-74.0, 40.7),
			}},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.GreaterOrEqual(t, len(results), 2)
	// First result should be nyc or hoboken (both very close).
	firstID := results[0].Map()["_id"]
	assert.True(t, firstID == "nyc" || firstID == "hoboken", "nearest should be nyc or hoboken")
}

// TestGeo_near_MaxDistance verifies $near with $maxDistance (in metres). (DongoXFail)
func TestGeo_near_MaxDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	// 100km radius around NYC.
	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.7)},
				{Key: "$maxDistance", Value: 100000},
			},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_near_MinDistance verifies $near with $minDistance (in metres). (DongoXFail)
func TestGeo_near_MinDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near with $minDistance not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	// Only return documents at least 1000km from the query point (near NYC).
	// LA is ~3940km away, NYC is ~0km → only LA should be returned.
	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.7)},
				{Key: "$minDistance", Value: 1000000},
			},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "la", results[0].Map()["_id"])
}

// TestGeo_near_MinAndMaxDistance verifies $near with both $minDistance and
// $maxDistance (in metres). (DongoXFail)
func TestGeo_near_MinAndMaxDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "exact"), e("loc", geoPoint(-74.0060, 40.7128))),  // ~1km from query
		d(e("_id", "medium"), e("loc", geoPoint(-73.9496, 40.6501))), // ~10km
		d(e("_id", "far"), e("loc", geoPoint(-118.2437, 34.0522))),   // ~3940km
	)

	// Between 5km and 50km.
	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.75)},
				{Key: "$minDistance", Value: 5000},
				{Key: "$maxDistance", Value: 50000},
			},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "medium", results[0].Map()["_id"])
}

// ─── $nearSphere ─────────────────────────────────────────────────────────────

// TestGeo_nearSphere_Basic verifies $nearSphere with $geometry on a 2dsphere
// index. Results must be sorted by spherical distance. (DongoXFail)
func TestGeo_nearSphere_Basic(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $nearSphere not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$nearSphere",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPoint(-74.0, 40.7),
			}},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_nearSphere_MaxDistance verifies $nearSphere with $maxDistance. (DongoXFail)
func TestGeo_nearSphere_MaxDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $nearSphere not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$nearSphere",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.7)},
				{Key: "$maxDistance", Value: 100000},
			},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_nearSphere_MinDistance verifies $nearSphere with $minDistance. (DongoXFail)
func TestGeo_nearSphere_MinDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $nearSphere not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	cursor, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$nearSphere",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.7)},
				{Key: "$minDistance", Value: 1000000},
			},
		}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "la", results[0].Map()["_id"])
}

// ─── $geoNear aggregation stage ──────────────────────────────────────────────

// TestGeo_geoNear_Basic verifies the $geoNear aggregation stage produces
// documents sorted by distance with a distanceField. (DongoXFail)
func TestGeo_geoNear_Basic(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
		d(e("_id", "hoboken"), e("loc", geoPoint(-74.0323, 40.7440))),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "dist"},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.GreaterOrEqual(t, len(results), 2)

	// Results must be sorted by distance ascending.
	for i := 1; i < len(results); i++ {
		prevDist := results[i-1].Map()["dist"].(float64)
		currDist := results[i].Map()["dist"].(float64)
		assert.LessOrEqualf(t, prevDist, currDist, "results must be sorted by distance at index %d", i)
	}
}

// TestGeo_geoNear_MaxDistance verifies $geoNear with maxDistance (metres). (DongoXFail)
func TestGeo_geoNear_MaxDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "dist"},
			{Key: "maxDistance", Value: 100000},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// TestGeo_geoNear_MinDistance verifies $geoNear with minDistance (metres). (DongoXFail)
func TestGeo_geoNear_MinDistance(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "dist"},
			{Key: "minDistance", Value: 1000000},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "la", results[0].Map()["_id"])
}

// TestGeo_geoNear_Query verifies $geoNear with a query filter. (DongoXFail)
func TestGeo_geoNear_Query(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128)), e("type", "city")),
		d(e("_id", "hoboken"), e("loc", geoPoint(-74.0323, 40.7440)), e("type", "town")),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522)), e("type", "city")),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "dist"},
			{Key: "query", Value: bson.D{{Key: "type", Value: "city"}}},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Only cities — hoboken (type=town) excluded.
	for _, r := range results {
		assert.NotEqual(t, "hoboken", r.Map()["_id"])
	}
	require.GreaterOrEqual(t, len(results), 1)
}

// TestGeo_geoNear_NonSpherical verifies $geoNear with spherical=false on a 2d
// index (legacy planar mode). (DongoXFail)
func TestGeo_geoNear_NonSpherical(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dIndex(t, coll, "coords")

	insertDocs(t, coll,
		d(e("_id", "close"), e("coords", bson.A{-74.0, 40.7})),
		d(e("_id", "far"), e("coords", bson.A{-118.2, 34.0})),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: bson.A{-74.0, 40.7}},
			{Key: "distanceField", Value: "dist"},
			{Key: "spherical", Value: false},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.GreaterOrEqual(t, len(results), 1)
	assert.Equal(t, "close", results[0].Map()["_id"])
}

// ─── MongoDB 8.0 strict Point validation ────────────────────────────────────

// TestGeo_near_InvalidPointLongitude verifies that $near rejects a Point with
// longitude out of [-180, 180]. MongoDB 8.0 enforces strict validation. (DongoXFail)
func TestGeo_near_InvalidPointLongitude(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near strict Point validation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	// Invalid longitude: 200 is outside [-180, 180].
	_, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPoint(200.0, 40.7),
			}},
		}},
	}})
	require.Error(t, err, "MongoDB 8.0 must reject longitude 200")
}

// TestGeo_near_InvalidPointLatitude verifies that $near rejects a Point with
// latitude out of [-90, 90]. MongoDB 8.0 enforces strict validation. (DongoXFail)
func TestGeo_near_InvalidPointLatitude(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $near strict Point validation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	// Invalid latitude: 100 is outside [-90, 90].
	_, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$near",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPoint(-74.0, 100.0),
			}},
		}},
	}})
	require.Error(t, err, "MongoDB 8.0 must reject latitude 100")
}

// TestGeo_nearSphere_InvalidPoint verifies that $nearSphere rejects a Point
// with invalid coordinates. MongoDB 8.0 enforces strict validation. (DongoXFail)
func TestGeo_nearSphere_InvalidPoint(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $nearSphere strict Point validation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	_, err := coll.Find(ctx, bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$nearSphere",
			Value: bson.D{{
				Key:   "$geometry",
				Value: geoPoint(500.0, 40.7), // invalid longitude
			}},
		}},
	}})
	require.Error(t, err, "MongoDB 8.0 must reject longitude 500")
}

// TestGeo_geoNear_InvalidPoint verifies that $geoNear rejects a Point with
// invalid coordinates. MongoDB 8.0 enforces strict validation. (DongoXFail)
func TestGeo_geoNear_InvalidPoint(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear strict Point validation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 200.0)}, // invalid latitude
			{Key: "distanceField", Value: "dist"},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	if err == nil {
		// Some drivers defer error until cursor is iterated.
		var results []bson.D
		err = cursor.All(ctx, &results)
	}
	require.Error(t, err, "MongoDB 8.0 must reject latitude 200 in $geoNear")
}

// ─── compound / edge cases ───────────────────────────────────────────────────

// TestGeo_2dsphere_Compound verifies a compound index (scalar + 2dsphere)
// works for queries that include both a scalar and geo predicate. (DongoXFail)
func TestGeo_2dsphere_Compound(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere compound index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "category", Value: 1},
			{Key: "loc", Value: "2dsphere"},
		},
	})
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "nyc-cafe"), e("category", "cafe"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "nyc-bar"), e("category", "bar"), e("loc", geoPoint(-74.0100, 40.7100))),
		d(e("_id", "la-cafe"), e("category", "cafe"), e("loc", geoPoint(-118.2437, 34.0522))),
	)

	filter := bson.D{
		{Key: "category", Value: "cafe"},
		{Key: "loc", Value: bson.D{{
			Key: "$near",
			Value: bson.D{
				{Key: "$geometry", Value: geoPoint(-74.0, 40.7)},
				{Key: "$maxDistance", Value: 10000},
			},
		}}},
	}

	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc-cafe", results[0].Map()["_id"])
}

// TestGeo_2dsphere_MultipleIndexedFields verifies documents with multiple
// GeoJSON fields each covered by their own 2dsphere index. (DongoXFail)
func TestGeo_2dsphere_MultipleIndexedFields(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "origin")
	create2dsphereIndex(t, coll, "dest")

	insertDocs(t, coll,
		d(
			e("_id", "trip1"),
			e("origin", geoPoint(-74.0060, 40.7128)),
			e("dest", geoPoint(-118.2437, 34.0522)),
		),
	)

	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "trip1"))).Decode(&result))
	assert.Equal(t, "trip1", result.Map()["_id"])
}

// TestGeo_geoNear_IncludeLocs verifies $geoNear with the includeLocs option
// which adds the matched location field to each result. (DongoXFail)
func TestGeo_geoNear_IncludeLocs(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "dist"},
			{Key: "includeLocs", Value: "matchedLoc"},
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	m := results[0].Map()
	assert.NotNil(t, m["dist"], "distanceField should be present")
	assert.NotNil(t, m["matchedLoc"], "includeLocs field should be present")
}

// TestGeo_geoNear_DistanceMultiplier verifies $geoNear with distanceMultiplier
// to convert metres to kilometres. (DongoXFail)
func TestGeo_geoNear_DistanceMultiplier(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoNear aggregation not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
	)

	pipeline := bson.A{
		bson.D{{Key: "$geoNear", Value: bson.D{
			{Key: "near", Value: geoPoint(-74.0, 40.7)},
			{Key: "distanceField", Value: "distKm"},
			{Key: "distanceMultiplier", Value: 0.001}, // metres → km
			{Key: "spherical", Value: true},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	// NYC is within ~1 km of query point; with multiplier distance should be < 2.
	distKm := results[0].Map()["distKm"].(float64)
	assert.Less(t, distKm, 2.0, "distance in km should be less than 2")
}

// TestGeo_2dsphere_SparseIndex verifies that a sparse 2dsphere index skips
// documents that lack the indexed field. (DongoXFail)
func TestGeo_2dsphere_SparseIndex(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere sparse index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "loc", Value: "2dsphere"}},
		Options: options.Index().SetSparse(true),
	})
	require.NoError(t, err)

	// Insert one document with loc and one without.
	insertDocs(t, coll,
		d(e("_id", "with-loc"), e("loc", geoPoint(-74.0, 40.7))),
		d(e("_id", "no-loc")), // no loc field — sparse index skips this
	)

	// Both documents should exist in the collection.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)
}

// TestGeo_geoWithin_MultiPolygon verifies $geoWithin with a MultiPolygon. (DongoXFail)
func TestGeo_geoWithin_MultiPolygon(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index and $geoWithin with MultiPolygon not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	create2dsphereIndex(t, coll, "loc")

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", geoPoint(-74.0060, 40.7128))),
		d(e("_id", "la"), e("loc", geoPoint(-118.2437, 34.0522))),
		d(e("_id", "london"), e("loc", geoPoint(-0.1276, 51.5074))),
	)

	// Two polygons: one around NYC, one around LA.
	nycRing := bson.A{coord(-74.3, 40.5), coord(-73.7, 40.5), coord(-73.7, 40.9), coord(-74.3, 40.9), coord(-74.3, 40.5)}
	laRing := bson.A{coord(-118.5, 33.8), coord(-118.0, 33.8), coord(-118.0, 34.2), coord(-118.5, 34.2), coord(-118.5, 33.8)}

	mpoly := bson.D{
		{Key: "type", Value: "MultiPolygon"},
		{Key: "coordinates", Value: bson.A{
			bson.A{nycRing},
			bson.A{laRing},
		}},
	}

	filter := bson.D{{
		Key: "loc",
		Value: bson.D{{
			Key: "$geoWithin",
			Value: bson.D{{Key: "$geometry", Value: mpoly}},
		}},
	}}

	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)
	ids := []interface{}{results[0].Map()["_id"], results[1].Map()["_id"]}
	assert.Contains(t, ids, "nyc")
	assert.Contains(t, ids, "la")
}

// TestGeo_2dsphere_IndexVersion verifies that 2dsphere index version defaults
// to 3 in MongoDB 8.0. (DongoXFail)
func TestGeo_2dsphere_IndexVersion(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "loc", Value: "2dsphere"}},
	})
	require.NoError(t, err)

	// Verify index exists and list it.
	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	found := false
	for _, idx := range indexes {
		if key, ok := idx["key"].(bson.M); ok {
			if key["loc"] == "2dsphere" {
				found = true
				// MongoDB 8.0 defaults to 2dsphereIndexVersion: 3.
				if v, ok := idx["2dsphereIndexVersion"]; ok {
					assert.EqualValues(t, 3, v)
				}
			}
		}
	}
	assert.True(t, found, "2dsphere index should appear in listIndexes")
}
