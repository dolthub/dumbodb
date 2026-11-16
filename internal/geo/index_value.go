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

package geo

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// PlanarProblem is why a 2d index cannot hold a value.
type PlanarProblem int

const (
	PlanarOK PlanarProblem = iota
	// PlanarMalformed is mongod's 16804: the value is not a legacy point.
	PlanarMalformed
	// PlanarSingleElement is mongod's 13068: a point of one coordinate.
	PlanarSingleElement
	// PlanarOutOfBounds is mongod's 13027: a coordinate outside the index's
	// bounds, or not finite.
	PlanarOutOfBounds
)

// PlanarIndexProblem reports whether a 2d index whose bounds are [min, max] can
// hold v, a field's value, as mongod decides it. A value that is not an array
// or document, or is empty, holds no point and is allowed. An array whose
// first element is an array or document holds one point per element.
func PlanarIndexProblem(v any, min, max float64) PlanarProblem {
	switch v := v.(type) {
	case *types.Array:
		if v.Len() == 0 {
			return PlanarOK
		}
		if !isComposite(must.NotFail(v.Get(0))) {
			return planarPoint(v, min, max)
		}
		for i := range v.Len() {
			if p := planarPoint(must.NotFail(v.Get(i)), min, max); p != PlanarOK {
				return p
			}
		}
		return PlanarOK
	case *types.Document:
		if v.Len() == 0 {
			return PlanarOK
		}
		return planarPoint(v, min, max)
	}
	return PlanarOK
}

func planarPoint(point any, min, max float64) PlanarProblem {
	coords := pointValues(point)
	switch {
	case coords == nil:
		return PlanarMalformed
	case len(coords) < 2:
		return PlanarSingleElement
	}
	for _, c := range coords[:2] {
		if _, isNumber := numericType(c); !isNumber {
			return PlanarMalformed
		}
	}
	for _, c := range coords[:2] {
		f, finite := number(c)
		if !finite || f < min || f > max {
			return PlanarOutOfBounds
		}
	}
	return PlanarOK
}

// pointValues returns the values of an array, or of a document in field
// order, or nil for any other value.
func pointValues(point any) []any {
	switch point := point.(type) {
	case *types.Array:
		values := make([]any, point.Len())
		for i := range values {
			values[i] = must.NotFail(point.Get(i))
		}
		return values
	case *types.Document:
		return point.Values()
	}
	return nil
}

func isComposite(v any) bool {
	switch v.(type) {
	case *types.Array, *types.Document:
		return true
	}
	return false
}

// SphereIndexProblem returns why a 2dsphere index on field cannot hold v, a
// field's value, in mongod's words, or "" when it can. Null, an empty array
// and an empty document hold no geometry and are allowed. Polygon edges are
// not checked for self-intersection.
func SphereIndexProblem(field string, v any) string {
	switch v := v.(type) {
	case types.NullType:
		return ""
	case *types.Array:
		if v.Len() == 0 {
			return ""
		}
		return legacySpherePoint(v, pointValues(v), false)
	case *types.Document:
		if v.Len() == 0 {
			return ""
		}
		if _, err := v.Get("type"); err != nil {
			// Stored documents have sorted keys, so which field is the
			// longitude is not known; either order may hold the point.
			return legacySpherePoint(v, v.Values(), true)
		}
		return geoJSONProblem(v)
	}
	return fmt.Sprintf("geo element must be an array or object: %s: %s", field, types.FormatAnyValue(v))
}

// legacySpherePoint checks the coordinates of point, a legacy point. anyOrder
// accepts them as latitude then longitude too.
func legacySpherePoint(point any, coords []any, anyOrder bool) string {
	if p := numericPair(coords); p != "" {
		return p
	}
	x, y, ok := numberPair(coords[0], coords[1])
	if !ok {
		return "Point coordinates must be finite numbers"
	}
	if !onSphere(x, y) && !(anyOrder && onSphere(y, x)) {
		return "can't project geometry into spherical CRS: " + types.FormatAnyValue(point)
	}
	return ""
}

// numericPair returns mongod's message when coords does not start with two
// numbers.
func numericPair(coords []any) string {
	for i := range 2 {
		var c any
		if i < len(coords) {
			c = coords[i]
		}
		if _, isNumber := numericType(c); !isNumber {
			return "Point must only contain numeric elements, instead got type " + bsonTypeName(c)
		}
	}
	return ""
}

func onSphere(lng, lat float64) bool {
	return lng >= -180 && lng <= 180 && lat >= -90 && lat <= 90
}

func geoJSONProblem(doc *types.Document) string {
	typ, _ := doc.Get("type")
	name, _ := typ.(string)
	if name == "GeometryCollection" {
		geometries, ok := getOrNull(doc, "geometries").(*types.Array)
		if !ok {
			return "GeometryCollection geometries must be an array, instead got type " +
				bsonTypeName(missingIfNull(getOrNull(doc, "geometries")))
		}
		if geometries.Len() == 0 {
			return "GeometryCollection geometries must have at least 1 element"
		}
		for i := range geometries.Len() {
			g, ok := must.NotFail(geometries.Get(i)).(*types.Document)
			if !ok {
				return "Element " + fmt.Sprint(i) + " of \"geometries\" is not an object"
			}
			if p := geoJSONProblem(g); p != "" {
				return p
			}
		}
		return ""
	}

	depth, known := map[string]int{
		"Point": 0, "LineString": 1, "MultiPoint": 1, "Polygon": 2, "MultiLineString": 2, "MultiPolygon": 3,
	}[name]
	if !known {
		return "unknown GeoJSON type: " + types.FormatAnyValue(doc)
	}
	coords := getOrNull(doc, "coordinates")
	if name == "Point" {
		if _, isDoc := coords.(*types.Document); !isDoc {
			if _, isArray := coords.(*types.Array); !isArray {
				return "Point must be an array or object, instead got type " + bsonTypeName(missingIfNull(coords))
			}
		}
	}
	return coordinatesProblem(name, coords, depth)
}

// coordinatesProblem checks coords, positions nested depth arrays deep.
func coordinatesProblem(typ string, coords any, depth int) string {
	if depth == 0 {
		return positionProblem(coords)
	}
	arr, ok := coords.(*types.Array)
	if !ok {
		return "GeoJSON coordinates must be an array"
	}
	switch {
	case arr.Len() == 0 && typ == "Polygon" && depth == 2:
		return "Polygon has no loops."
	case arr.Len() == 0 && depth == geoJSONDepth[typ]:
		return typ + " coordinates must have at least 1 element"
	case (typ == "LineString" || typ == "MultiLineString") && depth == 1:
		if n := distinctPositions(arr, 2); n < 2 {
			return fmt.Sprintf("GeoJSON LineString must have at least 2 vertices, instead got %d vertices: %s",
				n, types.FormatAnyValue(arr))
		}
	case (typ == "Polygon" || typ == "MultiPolygon") && depth == 1:
		if p := ringProblem(arr); p != "" {
			return p
		}
	}
	for i := range arr.Len() {
		if p := coordinatesProblem(typ, must.NotFail(arr.Get(i)), depth-1); p != "" {
			return p
		}
	}
	return ""
}

var geoJSONDepth = map[string]int{"MultiPoint": 1, "MultiLineString": 2, "MultiPolygon": 3}

// distinctPositions returns the number of distinct positions in arr, counting
// no further than limit, so a check for a few distinct vertices stays linear
// in the number of positions.
func distinctPositions(arr *types.Array, limit int) int {
	var distinct []any
	for i := range arr.Len() {
		v := must.NotFail(arr.Get(i))
		seen := false
		for _, d := range distinct {
			if types.Compare(d, v) == types.Equal {
				seen = true
				break
			}
		}
		if !seen {
			if distinct = append(distinct, v); len(distinct) == limit {
				break
			}
		}
	}
	return len(distinct)
}

func ringProblem(ring *types.Array) string {
	if ring.Len() < 4 {
		return fmt.Sprintf("Loop must have at least 3 different vertices, %d unique vertices were provided: %s",
			distinctPositions(ring, 3), types.FormatAnyValue(ring))
	}
	first, last := must.NotFail(ring.Get(0)), must.NotFail(ring.Get(ring.Len()-1))
	if types.Compare(first, last) != types.Equal {
		return "Loop is not closed, first vertex does not equal last vertex: " + types.FormatAnyValue(ring)
	}
	if n := distinctPositions(ring, 3); n < 3 {
		return fmt.Sprintf("Loop must have at least 3 different vertices, %d unique vertices were provided: %s",
			n, types.FormatAnyValue(ring))
	}
	return ""
}

// positionProblem checks a GeoJSON position: an array, or for a Point a
// document, starting with a longitude and a latitude.
func positionProblem(v any) string {
	coords := pointValues(v)
	if coords == nil {
		return "Point must be an array or object, instead got type " + bsonTypeName(missingIfNull(v))
	}
	if p := numericPair(coords); p != "" {
		return p
	}
	x, y, ok := numberPair(coords[0], coords[1])
	if !ok {
		return "Point coordinates must be finite numbers"
	}
	if !onSphere(x, y) {
		return fmt.Sprintf("Longitude/latitude is out of bounds, lng: %g lat: %g", x, y)
	}
	return ""
}

func missingIfNull(v any) any {
	if _, isNull := v.(types.NullType); isNull {
		return nil
	}
	return v
}

func getOrNull(doc *types.Document, key string) any {
	v, err := doc.Get(key)
	if err != nil {
		return types.Null
	}
	return v
}

func numericType(v any) (any, bool) {
	switch v.(type) {
	case float64, int32, int64, types.Decimal128:
		return v, true
	}
	return nil, false
}

func bsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "missing"
	case *types.Array:
		return "array"
	case *types.Document:
		return "object"
	case string:
		return "string"
	case bool:
		return "bool"
	case types.NullType:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}
