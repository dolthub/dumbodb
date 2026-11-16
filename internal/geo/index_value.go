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

	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
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
	return fmt.Sprintf("geo element must be an array or object: %s: %s", field, Echo(v))
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
		return "can't project geometry into spherical CRS: " + Echo(point)
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

// geoJSONProblem checks a GeoJSON object as mongod's parser does, in its
// order and words.
func geoJSONProblem(doc *types.Document) string {
	name, _ := fieldOrMissing(doc, "type").(string)
	coords := fieldOrMissing(doc, "coordinates")
	switch name {
	case "Point":
		return flatPointProblem(coords)
	case "LineString":
		return lineProblem(coords)
	case "MultiPoint":
		n, p := coordinatesArrayProblem(coords)
		if p == "" && n == 0 {
			return "MultiPoint coordinates must have at least 1 element"
		}
		return p
	case "Polygon":
		return polygonProblem(coords)
	case "MultiLineString":
		return eachProblem("MultiLineString", coords, lineProblem)
	case "MultiPolygon":
		return eachProblem("MultiPolygon", coords, polygonProblem)
	case "GeometryCollection":
		return geometryCollectionProblem(fieldOrMissing(doc, "geometries"))
	}
	return "unknown GeoJSON type: " + Echo(doc)
}

// eachProblem checks coords, the array of lines or polygons of a typ
// geometry, with check.
func eachProblem(typ string, coords any, check func(any) string) string {
	arr, ok := coords.(*types.Array)
	if !ok {
		return typ + " coordinates must be an array, instead got type " + bsonTypeName(coords)
	}
	for i := range arr.Len() {
		if p := check(must.NotFail(arr.Get(i))); p != "" {
			return p
		}
	}
	if arr.Len() == 0 {
		return typ + " coordinates must have at least 1 element"
	}
	return ""
}

func geometryCollectionProblem(geometries any) string {
	arr, ok := geometries.(*types.Array)
	if !ok {
		return "GeometryCollection geometries must be an array, instead got type " + bsonTypeName(geometries)
	}
	if arr.Len() == 0 {
		return "GeometryCollection geometries must have at least 1 element"
	}
	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))
		g, ok := elem.(*types.Document)
		if !ok {
			return fmt.Sprintf(`Element %d of "geometries" must be an object, instead got type %s: %s`,
				i, bsonTypeName(elem), Echo(elem))
		}
		if p := geoJSONProblem(g); p != "" {
			return p
		}
	}
	return ""
}

func lineProblem(coords any) string {
	n, p := coordinatesArrayProblem(coords)
	if p != "" {
		return p
	}
	if vertices := dedupedVertices(coords.(*types.Array), n); len(vertices) < 2 {
		return fmt.Sprintf("GeoJSON LineString must have at least 2 vertices, instead got %d vertices: %s",
			len(vertices), Echo(coords))
	}
	return ""
}

func polygonProblem(coords any) string {
	loops, ok := coords.(*types.Array)
	if !ok {
		return "Polygon coordinates must be an array, instead got type " + bsonTypeName(coords)
	}
	for i := range loops.Len() {
		if p := loopProblem(must.NotFail(loops.Get(i))); p != "" {
			return p
		}
	}
	if loops.Len() == 0 {
		return "Polygon has no loops."
	}
	return ""
}

func loopProblem(loop any) string {
	n, p := coordinatesArrayProblem(loop)
	if p != "" {
		return p
	}
	arr := loop.(*types.Array)
	if n == 0 {
		return "Loop has no vertices: " + Echo(loop)
	}
	if vertex(must.NotFail(arr.Get(0))) != vertex(must.NotFail(arr.Get(n-1))) {
		return "Loop is not closed, first vertex does not equal last vertex: " + Echo(loop)
	}
	vertices := dedupedVertices(arr, n)
	vertices = vertices[:len(vertices)-1]
	if len(vertices) < 3 {
		return fmt.Sprintf("Loop must have at least 3 different vertices, %d unique vertices were provided: %s",
			len(vertices), Echo(loop))
	}
	seen := make(map[[2]float64]int, len(vertices))
	for i, v := range vertices {
		if first, dup := seen[v]; dup {
			return fmt.Sprintf("Loop is not valid: %s Duplicate vertices: %d and %d", Echo(loop), first, i)
		}
		seen[v] = i
	}
	return ""
}

// coordinatesArrayProblem checks coords, an array of GeoJSON positions, and
// returns how many it holds.
func coordinatesArrayProblem(coords any) (int, string) {
	arr, ok := coords.(*types.Array)
	if !ok {
		return 0, "GeoJSON coordinates must be an array of coordinates, instead got type " + bsonTypeName(coords)
	}
	for i := range arr.Len() {
		if p := positionProblem(must.NotFail(arr.Get(i))); p != "" {
			return 0, p
		}
	}
	return arr.Len(), ""
}

// dedupedVertices returns the first n positions of arr, all valid, without
// adjacent repeats, as mongod counts a line's or loop's vertices.
func dedupedVertices(arr *types.Array, n int) [][2]float64 {
	var out [][2]float64
	for i := range n {
		v := vertex(must.NotFail(arr.Get(i)))
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// vertex returns the longitude and latitude of a valid GeoJSON position.
func vertex(position any) [2]float64 {
	values := pointValues(position)
	x, y, _ := numberPair(values[0], values[1])
	return [2]float64{x, y}
}

// positionProblem checks a GeoJSON position, which must be an array.
func positionProblem(v any) string {
	if _, ok := v.(*types.Array); !ok {
		return "GeoJSON coordinates must be an array, instead got type " + bsonTypeName(v)
	}
	return flatPointProblem(v)
}

// flatPointProblem checks a point given as an array or a document starting
// with a longitude and a latitude.
func flatPointProblem(v any) string {
	coords := pointValues(v)
	if coords == nil {
		return "Point must be an array or object, instead got type " + bsonTypeName(v)
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

// fieldOrMissing returns doc's field key, or nil when doc has none.
func fieldOrMissing(doc *types.Document, key string) any {
	v, err := doc.Get(key)
	if err != nil {
		return nil
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
	if v == nil {
		return "missing"
	}
	return handlerparams.AliasFromType(v)
}

// echoLimit bounds the bytes of a value an error repeats. mongod repeats a
// value in full; the bound is far above any realistic geometry and only cuts
// values large enough to bloat a reply.
const echoLimit = 1 << 20

// Echo formats v for an error message as mongod does, cut to echoLimit bytes.
func Echo(v any) string {
	return types.FormatAnyValueLimit(v, echoLimit)
}
