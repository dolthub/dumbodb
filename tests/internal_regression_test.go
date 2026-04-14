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

// Package tests contains dumbodb-specific regression and integration tests.
//
// MongoDB parity tests (compatibility between MongoDB and DumboDB) live in the
// dolthub/dumbodb-parity-testing repository. This package retains only tests for
// dumbodb-internal behaviors that have no MongoDB equivalent, such as internal
// resource management and implementation-specific edge cases.
//
// Run with:
//
//	go test -v ./tests/
package tests

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// buildOnce ensures the dumbodb binary is built exactly once per test run.
var buildOnce sync.Once

// dumboDBTestEnv holds a running dumbodb process and a connected MongoDB client.
type dumboDBTestEnv struct {
	cmd     *exec.Cmd
	client  *mongo.Client
	dataDir string
	port    int
}

// repoRoot returns the repository root directory (two levels above this file).
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..")
}

// startDumboDB launches a fresh dumbodb instance on a random free port.
// Optional extraArgs are appended to the dumbodb command line (e.g. "--auto-commit").
func startDumboDB(tb testing.TB, extraArgs ...string) *dumboDBTestEnv {
	tb.Helper()

	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := tb.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dolt")

	// Build once per test run to ensure the binary is up-to-date with current source.
	var buildErr error
	buildOnce.Do(func() {
		if mkErr := os.MkdirAll(filepath.Dir(binary), 0o755); mkErr != nil {
			buildErr = mkErr
			return
		}
		build := exec.Command("go", "build", "-o", binary, "./cmd/dumbodb/")
		build.Dir = repoRoot()
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("failed to build dumbodb: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		tb.Fatalf("failed to build dumbodb: %v", buildErr)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"--addr", addr, "--data-dir", dataDir}, extraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	require.NoError(tb, cmd.Start())

	env := &dumboDBTestEnv{
		cmd:     cmd,
		dataDir: dataDir,
		port:    port,
	}

	tb.Cleanup(func() {
		if env.client != nil {
			env.client.Disconnect(context.Background()) //nolint:errcheck
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	// Wait for dumbodb to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect with the MongoDB driver.
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/", addr)))
	require.NoError(tb, err)
	env.client = client

	return env
}

// collection returns a fresh collection for each test.
func (env *dumboDBTestEnv) collection(tb testing.TB) *mongo.Collection {
	tb.Helper()

	name := fmt.Sprintf("col_%d", rand.Int64())
	coll := env.client.Database("testdb").Collection(name)
	tb.Cleanup(func() {
		coll.Drop(context.Background()) //nolint:errcheck
	})

	return coll
}

// insertDocs inserts multiple documents and fails the test on error.
func insertDocs(tb testing.TB, coll *mongo.Collection, docs ...bson.D) {
	tb.Helper()

	iface := make([]interface{}, len(docs))
	for i, d := range docs {
		iface[i] = d
	}

	ctx := context.Background()
	_, err := coll.InsertMany(ctx, iface)
	require.NoError(tb, err)
}

// e is shorthand for bson.E (key-value pair in a bson.D document).
func e(key string, val interface{}) primitive.E {
	return primitive.E{Key: key, Value: val}
}

// d builds a bson.D from key-value pairs.
func d(elems ...primitive.E) bson.D {
	return bson.D(elems)
}

// TestBSON_array_nested is a regression test for nested array support (do-dor).
// MongoDB supports arrays containing arrays; dumbodb must store and retrieve them
// without error.
func TestBSON_array_nested(t *testing.T) {
	env := startDumboDB(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Insert a document that contains a nested array: { arr: [[1,2],[3,4]] }.
	doc := d(e("arr", bson.A{bson.A{int32(1), int32(2)}, bson.A{int32(3), int32(4)}}))
	_, err := coll.InsertOne(ctx, doc)
	require.NoError(t, err, "inserting a document with nested arrays must not fail")

	// Verify the document round-trips correctly.
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{}).Decode(&result))

	var arr bson.A
	for _, el := range result {
		if el.Key == "arr" {
			arr = el.Value.(bson.A)
			break
		}
	}
	require.NotNil(t, arr, "arr field must be present in the retrieved document")
	require.Len(t, arr, 2, "arr must have two sub-arrays")
}

// TestFind_CursorCleanupOnFilterError is a regression test for MultiCloser leaks.
//
// When a find command fails mid-iteration due to an invalid filter (e.g. a
// $jsonSchema with a malformed "required" clause), the cursor's underlying
// MultiCloser must be closed before the error is returned to the client.
// If it is not, GC will fire the finalizer and panic with:
//
//	panic: *iterator.MultiCloser has not been finalized
//
// This test does NOT run in parallel so that forcing GC here does not interact
// with other goroutines' resource tracking.
func TestFind_CursorCleanupOnFilterError(t *testing.T) {
	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "alice")),
		d(e("_id", int32(2)), e("name", "bob")),
	)

	ctx := context.Background()

	// $jsonSchema with required as a string (not an array) causes FilterDocument
	// to return an error on the first document, which fails ConsumeValuesN.
	// The fix: MsgFind calls h.cursors.CloseAndRemove before returning the error,
	// properly finalizing the MultiCloser.
	_, err := coll.Find(ctx, d(e("$jsonSchema", d(e("required", "not-an-array")))))
	require.Error(t, err)

	// Force GC multiple times to flush the finalizer goroutine.
	// If the MultiCloser leaked (Close not called), this will panic.
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
}

// TestQuery_bitsAllClear_bitmask verifies {field: {$bitsAllClear: bitmask}}.
// $bitsAllClear matches if ALL bits set in the bitmask are clear in the field.
func TestQuery_bitsAllClear_bitmask(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// flags: 0b0100 (4) — bits 0 and 1 are clear, bit 2 is set.
	// flags: 0b0001 (1) — bit 0 is set.
	// flags: 0b0000 (0) — all bits clear.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("flags", int32(4))),  // 0b0100
		d(e("_id", int32(2)), e("flags", int32(1))),  // 0b0001
		d(e("_id", int32(3)), e("flags", int32(0))),  // 0b0000
	)

	ctx := context.Background()
	// $bitsAllClear: 5 (0b0101) — both bits 0 and 2 must be clear.
	// Only flags=0 (doc3) satisfies this; doc1 has bit2 set, doc2 has bit0 set.
	cursor, err := coll.Find(ctx, d(e("flags", d(e("$bitsAllClear", int32(5))))))
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(3), gotID)
}

// TestQuery_bitsAnySet_positions verifies {field: {$bitsAnySet: [pos1, pos2]}} (position array form).
// $bitsAnySet matches if ANY of the specified bit positions are set in the field.
func TestQuery_bitsAnySet_positions(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// flags: 0b0001 (1) — bit 0 set.
	// flags: 0b1000 (8) — bit 3 set.
	// flags: 0b0100 (4) — bit 2 set.
	// flags: 0b0000 (0) — no bits set.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("flags", int32(1))),
		d(e("_id", int32(2)), e("flags", int32(8))),
		d(e("_id", int32(3)), e("flags", int32(4))),
		d(e("_id", int32(4)), e("flags", int32(0))),
	)

	ctx := context.Background()
	// $bitsAnySet: [0, 3] — match if bit 0 OR bit 3 is set.
	// Matches doc1 (bit0) and doc2 (bit3). doc3 has only bit2; doc4 has none.
	cursor, err := coll.Find(ctx, d(e("flags", d(e("$bitsAnySet", bson.A{int32(0), int32(3)})))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	ids := make([]int32, len(results))
	for i, r := range results {
		for _, el := range r {
			if el.Key == "_id" {
				ids[i] = el.Value.(int32)
			}
		}
	}
	require.Equal(t, []int32{1, 2}, ids)
}

// TestQuery_geo_within_box verifies {field: {$geoWithin: {$box: [[x1,y1],[x2,y2]]}}}
// matches documents whose coordinate lies inside the bounding box.
func TestQuery_geo_within_box(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// Store coordinates as legacy [lon, lat] arrays.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", bson.A{float64(5), float64(5)})),   // inside
		d(e("_id", int32(2)), e("loc", bson.A{float64(15), float64(5)})),  // outside (x > 10)
		d(e("_id", int32(3)), e("loc", bson.A{float64(2), float64(2)})),   // inside
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$geoWithin", d(e("$box", bson.A{
			bson.A{float64(0), float64(0)},
			bson.A{float64(10), float64(10)},
		})))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	ids := make([]int32, len(results))
	for i, r := range results {
		for _, el := range r {
			if el.Key == "_id" {
				ids[i] = el.Value.(int32)
			}
		}
	}
	require.Equal(t, []int32{1, 3}, ids)
}

// TestQuery_proj_slice_first_n verifies $slice first-N projection in a Find call.
func TestQuery_proj_slice_first_n(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", int32(2)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var arr bson.A
	for _, el := range results[0] {
		if el.Key == "nums" {
			arr = el.Value.(bson.A)
		}
	}
	require.NotNil(t, arr)
	require.Equal(t, bson.A{int32(10), int32(20)}, arr)
}

// TestQuery_jsonSchema_required_invalid verifies that $jsonSchema with a required array
// containing a non-string element returns a command error.
func TestQuery_jsonSchema_required_invalid(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
	)

	ctx := context.Background()
	// required must be an array of strings; passing an integer element should error.
	_, err := coll.Find(ctx,
		d(e("$jsonSchema", d(
			e("required", bson.A{int32(42)}),
		))),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "required")
}

// TestQuery_type_decimal verifies {field: {$type: "decimal"}} matches Decimal128 values.
func TestQuery_type_decimal(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	decVal, decErr := primitive.ParseDecimal128("3.14")
	require.NoError(t, decErr)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("val", decVal)),
		d(e("_id", int32(2)), e("val", int32(42))),
		d(e("_id", int32(3)), e("val", "text")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("val", d(e("$type", "decimal")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_within_polygon verifies {field: {$geoWithin: {$polygon: [...]}}}
// matches documents whose coordinate lies inside the polygon.
func TestQuery_geo_within_polygon(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", bson.A{float64(2), float64(2)})),   // inside triangle
		d(e("_id", int32(2)), e("loc", bson.A{float64(20), float64(20)})), // outside
	)

	ctx := context.Background()
	// Triangle: (0,0), (10,0), (0,10). Point (2,2): 2+2=4 < 10, inside.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$geoWithin", d(e("$polygon", bson.A{
			bson.A{float64(0), float64(0)},
			bson.A{float64(10), float64(0)},
			bson.A{float64(0), float64(10)},
		})))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_within_centerSphere verifies {field: {$geoWithin: {$centerSphere: [[lon,lat], radiusRad]}}}.
func TestQuery_geo_within_centerSphere(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// New York (approx): lon=-74, lat=40.7
	// London (approx):   lon=0,   lat=51.5
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", bson.A{float64(-74), float64(40.7)})),
		d(e("_id", int32(2)), e("loc", bson.A{float64(0), float64(51.5)})),
	)

	ctx := context.Background()
	// Center at NYC, radius ~0.1 radians ≈ 637 km — should include NYC but not London.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$geoWithin", d(e("$centerSphere", bson.A{
			bson.A{float64(-74), float64(40.7)},
			float64(0.1),
		})))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_near verifies {field: {$near: {$geometry: {type:"Point", coordinates:[lon,lat]}, $maxDistance: n}}}
// returns documents within maxDistance meters and sorted by ascending distance.
func TestQuery_geo_near(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// Place two points: one near [0,0], one far away.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(0), float64(0)})))),
		d(e("_id", int32(2)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(90), float64(0)})))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$near", d(
			e("$geometry", d(e("type", "Point"), e("coordinates", bson.A{float64(0.001), float64(0)}))),
			e("$maxDistance", float64(200000)), // 200 km
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_nearSphere verifies {field: {$nearSphere: {$geometry: {...}, $maxDistance: n}}}.
func TestQuery_geo_nearSphere(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(0), float64(0)})))),
		d(e("_id", int32(2)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(90), float64(0)})))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$nearSphere", d(
			e("$geometry", d(e("type", "Point"), e("coordinates", bson.A{float64(0.001), float64(0)}))),
			e("$maxDistance", float64(200000)),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_nearSphere_legacy2d verifies {field: {$nearSphere: [lon, lat], $maxDistance: radians}}
// on a plain 2d index (legacy coordinates). $maxDistance is in radians; dumbodb must convert to metres
// before applying the haversine filter. Regression for do-twgm.
func TestQuery_geo_nearSphere_legacy2d(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	// Two points near the origin.  In radians the great-circle radius of ~111 km is ~0.0175.
	// doc1 is at (0,0) — ~111 km from query point (1,0).
	// doc2 is at (0.5,0) — ~55 km from query point (1,0).
	// doc3 is at (10,0) — ~1000 km from query point (1,0), well outside 0.0175 rad.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", bson.A{float64(0), float64(0)})),
		d(e("_id", int32(2)), e("loc", bson.A{float64(0.5), float64(0)})),
		d(e("_id", int32(3)), e("loc", bson.A{float64(10), float64(0)})),
	)

	ctx := context.Background()
	// maxDistance = 0.0175 radians ≈ 111 km — should include doc1 and doc2, not doc3.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(
			e("$nearSphere", bson.A{float64(1), float64(0)}),
			e("$maxDistance", float64(0.0175)),
		))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)
}

// TestQuery_geo_intersects_point verifies {field: {$geoIntersects: {$geometry: {type:"Point",...}}}}
// matches documents that contain the exact point.
func TestQuery_geo_intersects_point(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(1), float64(1)})))),
		d(e("_id", int32(2)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(2), float64(2)})))),
	)

	ctx := context.Background()
	// Query for exact point [1, 1] — should match doc1 only.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$geoIntersects", d(e("$geometry",
			d(e("type", "Point"), e("coordinates", bson.A{float64(1), float64(1)})),
		)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_geo_intersects_polygon verifies $geoIntersects with a Polygon geometry
// matches documents whose geometry lies within/intersects the polygon.
func TestQuery_geo_intersects_polygon(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(5), float64(5)})))),
		d(e("_id", int32(2)), e("loc", d(e("type", "Point"), e("coordinates", bson.A{float64(50), float64(50)})))),
	)

	ctx := context.Background()
	// Square polygon (0,0)→(10,0)→(10,10)→(0,10)→(0,0). Point (5,5) is inside; (50,50) is outside.
	cursor, err := coll.Find(ctx,
		d(e("loc", d(e("$geoIntersects", d(e("$geometry", d(
			e("type", "Polygon"),
			e("coordinates", bson.A{bson.A{
				bson.A{float64(0), float64(0)},
				bson.A{float64(10), float64(0)},
				bson.A{float64(10), float64(10)},
				bson.A{float64(0), float64(10)},
				bson.A{float64(0), float64(0)},
			}}),
		))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_elemMatch_embedded_docs verifies {field: {$elemMatch: {a:1, b:2}}}
// matches arrays of embedded documents (do-l5if: field-condition $elemMatch fix).
func TestQuery_elemMatch_embedded_docs(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("items", bson.A{
			d(e("a", int32(1)), e("b", int32(2))),
			d(e("a", int32(3)), e("b", int32(4))),
		})),
		d(e("_id", int32(2)), e("items", bson.A{
			d(e("a", int32(5)), e("b", int32(6))),
		})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("items", d(e("$elemMatch", d(e("a", int32(1)), e("b", int32(2))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_elemMatch_embedded_multi_cond verifies $elemMatch with multiple operator conditions
// on a single embedded document field (do-l5if: field-condition $elemMatch fix).
func TestQuery_elemMatch_embedded_multi_cond(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("items", bson.A{
			d(e("score", int32(7))),
		})),
		d(e("_id", int32(2)), e("items", bson.A{
			d(e("score", int32(3))),
		})),
		d(e("_id", int32(3)), e("items", bson.A{
			d(e("score", int32(11))),
		})),
	)

	ctx := context.Background()
	// Match items where score > 5 AND score < 10.
	cursor, err := coll.Find(ctx,
		d(e("items", d(e("$elemMatch", d(
			e("score", d(e("$gt", int32(5)), e("$lt", int32(10)))),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	require.Equal(t, int32(1), gotID)
}

// TestQuery_proj_slice_last_n verifies $slice last-N projection in a Find call.
func TestQuery_proj_slice_last_n(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", int32(-2)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var arr bson.A
	for _, el := range results[0] {
		if el.Key == "nums" {
			arr = el.Value.(bson.A)
		}
	}
	require.NotNil(t, arr)
	require.Equal(t, bson.A{int32(30), int32(40)}, arr)
}

// TestQuery_proj_slice_skip_limit verifies $slice skip+limit projection in a Find call.
func TestQuery_proj_slice_skip_limit(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40), int32(50)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", bson.A{int32(2), int32(2)}))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var arr bson.A
	for _, el := range results[0] {
		if el.Key == "nums" {
			arr = el.Value.(bson.A)
		}
	}
	require.NotNil(t, arr)
	require.Equal(t, bson.A{int32(30), int32(40)}, arr)
}

// TestQuery_type_number_alias_decimal verifies {field: {$type: "number"}} matches Decimal128 values.
// Regression for do-74lo: the 'number' alias was not matching Decimal128.
func TestQuery_type_number_alias_decimal(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	decVal, decErr := primitive.ParseDecimal128("9.99")
	require.NoError(t, decErr)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("val", float64(1.5))),
		d(e("_id", int32(2)), e("val", int32(42))),
		d(e("_id", int32(3)), e("val", int64(100))),
		d(e("_id", int32(4)), e("val", decVal)),
		d(e("_id", int32(5)), e("val", "text")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("val", d(e("$type", "number")))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	ids := make([]int32, len(results))
	for i, r := range results {
		for _, el := range r {
			if el.Key == "_id" {
				ids[i] = el.Value.(int32)
			}
		}
	}
	require.Equal(t, []int32{1, 2, 3, 4}, ids)
}

// TestQuery_type_objectid verifies {field: {$type: "objectId"}} matches documents
// where the field value is an ObjectID.
func TestQuery_type_objectid(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("ref", primitive.NewObjectID())),
		d(e("_id", int32(2)), e("ref", "not-an-oid")),
		d(e("_id", int32(3)), e("ref", primitive.NewObjectID())),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("ref", d(e("$type", "objectId")))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	ids := make([]int32, len(results))
	for i, r := range results {
		for _, el := range r {
			if el.Key == "_id" {
				ids[i] = el.Value.(int32)
			}
		}
	}
	require.Equal(t, []int32{1, 3}, ids)
}

