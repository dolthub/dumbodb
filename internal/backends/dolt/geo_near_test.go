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

package dolt

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func geoPointDoc(id int32, x, y float64) *types.Document {
	return must.NotFail(types.NewDocument("_id", id, "loc", must.NotFail(types.NewDocument(
		"type", "Point", "coordinates", must.NotFail(types.NewArray(x, y)),
	))))
}

func haversine(x1, y1, x2, y2 float64) float64 {
	const r = math.Pi / 180
	a := math.Pow(math.Sin((y2-y1)*r/2), 2) + math.Cos(y1*r)*math.Cos(y2*r)*math.Pow(math.Sin((x2-x1)*r/2), 2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// geoDistance measures a document's loc as the handler does: a point's
// distance, math.MaxFloat64 for anything else.
func geoDistance(x, y float64, spherical bool) func(*types.Document) float64 {
	return func(doc *types.Document) float64 {
		v, err := doc.Get("loc")
		if err != nil {
			return math.MaxFloat64
		}
		px, py, ok := geoPoint(v)
		if !ok {
			return math.MaxFloat64
		}
		if spherical {
			return haversine(x, y, px, py)
		}
		return math.Hypot(px-x, py-y)
	}
}

func newGeoCollection(t *testing.T, kp backends.IndexKeyPair, docs []*types.Document) *collection {
	t.Helper()
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "loc", Key: []backends.IndexKeyPair{kp}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	return c
}

// nearIDs reads up to n documents nearest (x, y), returning their ids and
// the documents fetched.
func nearIDs(t *testing.T, c *collection, params *backends.GeoNearParams, n int) ([]int32, int64) {
	t.Helper()
	ctx, examined := backends.WithExaminedCounts(context.Background())
	res, err := c.GeoNear(ctx, params)
	if err != nil || res == nil {
		t.Fatalf("GeoNear: %v, %v", res, err)
	}
	defer res.Iter.Close()
	var ids []int32
	for len(ids) < n {
		_, doc, err := res.Iter.Next()
		if err != nil {
			break
		}
		ids = append(ids, must.NotFail(doc.Get("_id")).(int32))
	}
	return ids, examined.Docs()
}

// bruteNear sorts docs by dist, ties by insertion, and returns the first n ids.
func bruteNear(docs []*types.Document, dist func(*types.Document) float64, maxDist float64, n int) []int32 {
	type pair struct {
		id int32
		d  float64
	}
	var pairs []pair
	for _, d := range docs {
		if dd := dist(d); dd <= maxDist {
			pairs = append(pairs, pair{must.NotFail(d.Get("_id")).(int32), dd})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].d < pairs[j].d })
	var out []int32
	for i := 0; i < len(pairs) && i < n; i++ {
		out = append(out, pairs[i].id)
	}
	return out
}

func sameDistances(t *testing.T, got, want []int32, byID map[int32]*types.Document, dist func(*types.Document) float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d documents %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if g, w := dist(byID[got[i]]), dist(byID[want[i]]); g != w {
			t.Fatalf("result %d: id %d at %v, want id %d at %v", i, got[i], g, want[i], w)
		}
	}
}

func TestGeoNearSphereStreamsNearestFirst(t *testing.T) {
	var docs []*types.Document
	for i := int32(0); i < 10000; i++ {
		docs = append(docs, geoPointDoc(i+1, float64(i%100)*0.01, float64(i/100)*0.01))
	}
	// Across the antimeridian and near the north pole.
	docs = append(docs,
		geoPointDoc(20001, 179.99, 10), geoPointDoc(20002, -179.99, 10), geoPointDoc(20003, 30, 89.999),
		must.NotFail(types.NewDocument("_id", int32(20004), "loc", must.NotFail(types.NewDocument(
			"type", "LineString", "coordinates", must.NotFail(types.NewArray(
				must.NotFail(types.NewArray(0.0, 0.0)), must.NotFail(types.NewArray(1.0, 1.0)),
			)),
		)))),
	)
	byID := make(map[int32]*types.Document, len(docs))
	for _, d := range docs {
		byID[must.NotFail(d.Get("_id")).(int32)] = d
	}
	c := newGeoCollection(t, backends.IndexKeyPair{Field: "loc", Geo2DSphere: true}, docs)

	for _, tc := range []struct {
		name    string
		x, y    float64
		n       int
		maxDist float64
	}{
		{"grid", 0.5, 0.5, 10, math.Inf(1)},
		{"antimeridian", 179.995, 10, 2, math.Inf(1)},
		{"pole", -150, 89.99, 1, math.Inf(1)},
		{"max distance", 0.5, 0.5, 100000, 2500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dist := geoDistance(tc.x, tc.y, true)
			got, fetched := nearIDs(t, c, &backends.GeoNearParams{X: tc.x, Y: tc.y, Spherical: true, MaxDistance: tc.maxDist, Distance: dist}, tc.n)
			if !math.IsInf(tc.maxDist, 1) {
				var within []int32
				for _, id := range got {
					if dist(byID[id]) <= tc.maxDist {
						within = append(within, id)
					}
				}
				got = within
			}
			sameDistances(t, got, bruteNear(docs, dist, tc.maxDist, tc.n), byID, dist)
			if tc.name == "grid" && fetched > 1000 {
				t.Errorf("fetched %d documents for %d nearest", fetched, tc.n)
			}
		})
	}

	t.Run("a non-point geometry comes last", func(t *testing.T) {
		got, _ := nearIDs(t, c, &backends.GeoNearParams{X: 0.5, Y: 0.5, Spherical: true, Distance: geoDistance(0.5, 0.5, true)}, len(docs))
		if len(got) != len(docs) || got[len(got)-1] != 20004 {
			t.Errorf("read %d documents, last %d; want all %d, the LineString last", len(got), got[len(got)-1], len(docs))
		}
	})
}

func TestGeoNear2dPlanar(t *testing.T) {
	var docs []*types.Document
	for i := int32(0); i < 2500; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i+1, "loc", must.NotFail(types.NewArray(float64(i%50), float64(i/50))))))
	}
	byID := make(map[int32]*types.Document, len(docs))
	for _, d := range docs {
		byID[must.NotFail(d.Get("_id")).(int32)] = d
	}
	c := newGeoCollection(t, backends.IndexKeyPair{Field: "loc", Geo2D: true}, docs)
	dist := geoDistance(20.3, 31.7, false)
	got, fetched := nearIDs(t, c, &backends.GeoNearParams{X: 20.3, Y: 31.7, Distance: dist}, 25)
	sameDistances(t, got, bruteNear(docs, dist, math.Inf(1), 25), byID, dist)
	if fetched > 500 {
		t.Errorf("fetched %d documents for 25 nearest", fetched)
	}
}

// TestGeoIndexServesNoOtherRead checks a geo index, whose keys are cells, is
// not read for a range or covered read of its field.
func TestGeoIndexServesNoOtherRead(t *testing.T) {
	c := newGeoCollection(t, backends.IndexKeyPair{Field: "loc", Geo2D: true}, []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "loc", must.NotFail(types.NewArray(1.0, 2.0)))),
	})
	res, plan, err := c.query(context.Background(), &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("loc", must.NotFail(types.NewArray(1.0, 2.0)))),
		Fields: []string{"loc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Iter.Close()
	if got := planChain(plan); len(got) != 1 || got[0] != "COLLSCAN" {
		t.Errorf("plan %v, want COLLSCAN", got)
	}
}

// A non-finite point or bound declines the index read, and a cancelled
// context stops one, so neither walks the grid without end.
func TestGeoNearRefusesNonFinite(t *testing.T) {
	docs := []*types.Document{geoPointDoc(1, 0, 0), geoPointDoc(2, 1, 1)}
	sphere := newGeoCollection(t, backends.IndexKeyPair{Field: "loc", Geo2DSphere: true}, docs)
	planar := newGeoCollection(t, backends.IndexKeyPair{Field: "loc", Geo2D: true}, []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "loc", must.NotFail(types.NewArray(1.0, 1.0)))),
	})
	nan, inf := math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name   string
		c      *collection
		params backends.GeoNearParams
	}{
		{"sphere NaN x", sphere, backends.GeoNearParams{X: nan, Spherical: true, MaxDistance: inf}},
		{"sphere Inf y", sphere, backends.GeoNearParams{Y: inf, Spherical: true, MaxDistance: inf}},
		{"planar NaN x", planar, backends.GeoNearParams{X: nan, MaxDistance: inf}},
		{"planar Inf x", planar, backends.GeoNearParams{X: inf, MaxDistance: inf}},
		{"NaN max distance", sphere, backends.GeoNearParams{Spherical: true, MaxDistance: nan}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params.Distance = func(*types.Document) float64 { return 0 }
			res, err := tc.c.GeoNear(context.Background(), &tc.params)
			if err != nil || res != nil {
				t.Fatalf("GeoNear = %v, %v; want it declined", res, err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	res, err := sphere.GeoNear(ctx, &backends.GeoNearParams{Spherical: true, MaxDistance: inf, Distance: geoDistance(0, 0, true)})
	if err != nil || res == nil {
		t.Fatalf("GeoNear = %v, %v", res, err)
	}
	defer res.Iter.Close()
	cancel()
	if _, _, err := res.Iter.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("Next after cancel = %v, want context.Canceled", err)
	}
}
