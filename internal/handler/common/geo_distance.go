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

package common

import (
	"math"

	"github.com/dolthub/dumbodb/internal/geo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// GeoDistance returns the distance from the query point (x, y) to the nearest
// point of v, a stored location, or math.MaxFloat64 when v is not one. A
// location is a legacy [x, y] pair, a legacy {x, y} document (its first two
// fields in stored order), an array of either (each a location of the same
// document), or a GeoJSON geometry. Spherical distances are in metres on
// MongoDB's Earth radius, measured as S2 does; a point inside a polygon is at
// distance 0.
//
// A distance that overflows to infinity or NaN, as from coordinates near the
// float64 limit, is reported as MaxFloat64: no location.
func GeoDistance(v any, x, y float64, spherical bool) float64 {
	if d := geoDistance(v, x, y, spherical); !math.IsNaN(d) && !math.IsInf(d, 0) {
		return d
	}
	return math.MaxFloat64
}

func geoDistance(v any, x, y float64, spherical bool) float64 {
	if px, py, ok := geo.Point(v); ok {
		return pointDistance(x, y, px, py, spherical)
	}
	switch v := v.(type) {
	case *types.Document:
		if t, _ := v.Get("type"); t != nil {
			return geometryDistance(v, x, y, spherical)
		}
	case *types.Array:
		best := math.MaxFloat64
		for i := 0; i < v.Len(); i++ {
			best = min(best, GeoDistance(must.NotFail(v.Get(i)), x, y, spherical))
		}
		return best
	}
	return math.MaxFloat64
}

func pointDistance(x, y, px, py float64, spherical bool) float64 {
	if spherical {
		return angle(unitVector(x, y), unitVector(px, py)) * earthRadiusMeters
	}
	return euclidean(x, y, px, py)
}

// geometryDistance measures to a GeoJSON geometry.
func geometryDistance(g *types.Document, x, y float64, spherical bool) float64 {
	typ, _ := g.Get("type")
	if typ == "GeometryCollection" {
		geoms, _ := g.Get("geometries")
		return GeoDistance(geoms, x, y, spherical)
	}
	coords, _ := g.Get("coordinates")
	arr, ok := coords.(*types.Array)
	if !ok {
		return math.MaxFloat64
	}
	switch typ {
	case "Point":
		if px, py, ok := geo.Point(arr); ok {
			return pointDistance(x, y, px, py, spherical)
		}
	case "MultiPoint":
		return GeoDistance(arr, x, y, spherical)
	case "LineString":
		return lineDistance(arr, x, y, spherical)
	case "MultiLineString":
		return eachDistance(arr, x, y, func(a *types.Array) float64 { return lineDistance(a, x, y, spherical) })
	case "Polygon":
		return polygonDistance(arr, x, y, spherical)
	case "MultiPolygon":
		return eachDistance(arr, x, y, func(a *types.Array) float64 { return polygonDistance(a, x, y, spherical) })
	}
	return math.MaxFloat64
}

func eachDistance(arr *types.Array, x, y float64, dist func(*types.Array) float64) float64 {
	best := math.MaxFloat64
	for i := 0; i < arr.Len(); i++ {
		if a, ok := must.NotFail(arr.Get(i)).(*types.Array); ok {
			best = min(best, dist(a))
		}
	}
	return best
}

// lineDistance measures to the nearest point of a line's edges.
func lineDistance(line *types.Array, x, y float64, spherical bool) float64 {
	pts := linePoints(line)
	if len(pts) == 0 {
		return math.MaxFloat64
	}
	best := pointDistance(x, y, pts[0][0], pts[0][1], spherical)
	for i := 1; i < len(pts); i++ {
		best = min(best, edgeDistance(x, y, pts[i-1], pts[i], spherical))
	}
	return best
}

// polygonDistance is 0 inside the polygon (its outer ring less its holes),
// else the distance to its nearest edge.
func polygonDistance(rings *types.Array, x, y float64, spherical bool) float64 {
	inside := false
	for i := 0; i < rings.Len(); i++ {
		ring, ok := must.NotFail(rings.Get(i)).(*types.Array)
		if !ok {
			continue
		}
		in := ringContains(linePoints(ring), x, y)
		if i == 0 {
			inside = in
		} else if in {
			inside = false
		}
	}
	if inside {
		return 0
	}
	return eachDistance(rings, x, y, func(a *types.Array) float64 { return lineDistance(a, x, y, spherical) })
}

func linePoints(line *types.Array) [][2]float64 {
	pts := make([][2]float64, 0, line.Len())
	for i := 0; i < line.Len(); i++ {
		if px, py, ok := geo.Point(must.NotFail(line.Get(i))); ok {
			pts = append(pts, [2]float64{px, py})
		}
	}
	return pts
}

// ringContains reports whether (x, y) lies inside ring, by the even-odd rule.
func ringContains(ring [][2]float64, x, y float64) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a[1] > y) != (b[1] > y) && x < (b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0] {
			in = !in
		}
	}
	return in
}

// edgeDistance measures to the nearest point of the edge from a to b: a
// great-circle arc on the sphere, a segment in the plane.
func edgeDistance(x, y float64, a, b [2]float64, spherical bool) float64 {
	if !spherical {
		dx, dy := b[0]-a[0], b[1]-a[1]
		t := 0.0
		if l := dx*dx + dy*dy; l > 0 {
			t = max(0, min(1, ((x-a[0])*dx+(y-a[1])*dy)/l))
		}
		return euclidean(x, y, a[0]+t*dx, a[1]+t*dy)
	}
	p, va, vb := unitVector(x, y), unitVector(a[0], a[1]), unitVector(b[0], b[1])
	n := cross(va, vb)
	if vecNorm(n) == 0 {
		return min(angle(p, va), angle(p, vb)) * earthRadiusMeters
	}
	// The nearest point lies inside the arc when p falls between its ends.
	if dot(cross(va, p), n) >= 0 && dot(cross(p, vb), n) >= 0 {
		return math.Abs(math.Asin(dot(p, n)/vecNorm(n))) * earthRadiusMeters
	}
	return min(angle(p, va), angle(p, vb)) * earthRadiusMeters
}

type vec3 [3]float64

func unitVector(lon, lat float64) vec3 {
	phi, theta := lat*math.Pi/180, lon*math.Pi/180
	return vec3{math.Cos(phi) * math.Cos(theta), math.Cos(phi) * math.Sin(theta), math.Sin(phi)}
}

func cross(a, b vec3) vec3 {
	return vec3{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}

func dot(a, b vec3) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func vecNorm(a vec3) float64 { return math.Sqrt(dot(a, a)) }

// angle is the angle between unit vectors, as S2 measures it.
func angle(a, b vec3) float64 { return math.Atan2(vecNorm(cross(a, b)), dot(a, b)) }
