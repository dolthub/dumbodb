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
	// PlanarSingleElement is mongod's 13068: an array of one element.
	PlanarSingleElement
	// PlanarOutOfBounds is mongod's 13027: a coordinate outside [-180, 180].
	PlanarOutOfBounds
)

// PlanarIndexProblem reports whether a 2d index can hold v, a field's value,
// as mongod decides it. A value that is not an array or document, or is empty,
// holds no point and is allowed. An array of arrays holds one point per
// element.
func PlanarIndexProblem(v any) PlanarProblem {
	switch v := v.(type) {
	case *types.Array:
		if v.Len() == 0 {
			return PlanarOK
		}
		if _, nested := must.NotFail(v.Get(0)).(*types.Array); nested {
			for i := range v.Len() {
				if p := PlanarIndexProblem(must.NotFail(v.Get(i))); p != PlanarOK {
					return p
				}
			}
			return PlanarOK
		}
		if v.Len() == 1 {
			return PlanarSingleElement
		}
		return planarPoint(must.NotFail(v.Get(0)), must.NotFail(v.Get(1)))
	case *types.Document:
		switch v.Len() {
		case 0:
			return PlanarOK
		case 1:
			return PlanarSingleElement
		}
		return planarPoint(must.NotFail(v.Get(v.Keys()[0])), must.NotFail(v.Get(v.Keys()[1])))
	}
	return PlanarOK
}

func planarPoint(a, b any) PlanarProblem {
	x, okX := number(a)
	y, okY := number(b)
	if !okX || !okY {
		return PlanarMalformed
	}
	if x < -180 || x > 180 || y < -180 || y > 180 {
		return PlanarOutOfBounds
	}
	return PlanarOK
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
		return legacySpherePoint(v)
	case *types.Document:
		if v.Len() == 0 {
			return ""
		}
		if _, err := v.Get("type"); err != nil {
			// A legacy point document; stored documents have sorted keys,
			// so which field is x is not known.
			return ""
		}
		return geoJSONProblem(v)
	}
	return fmt.Sprintf("geo element must be an array or object: %s: %s", field, types.FormatAnyValue(v))
}

func legacySpherePoint(arr *types.Array) string {
	for i := range 2 {
		var elem any
		if i < arr.Len() {
			elem = must.NotFail(arr.Get(i))
		}
		if _, ok := number(elem); !ok {
			if _, isNumber := numericType(elem); isNumber {
				continue
			}
			return "Point must only contain numeric elements, instead got type " + bsonTypeName(elem)
		}
	}
	x, y, ok := numberPair(must.NotFail(arr.Get(0)), must.NotFail(arr.Get(1)))
	if !ok || x < -180 || x > 180 || y < -90 || y > 90 {
		return "can't project geometry into spherical CRS: " + types.FormatAnyValue(arr)
	}
	return ""
}

func geoJSONProblem(doc *types.Document) string {
	typ, _ := doc.Get("type")
	name, _ := typ.(string)
	if name == "GeometryCollection" {
		geometries, ok := getOrNull(doc, "geometries").(*types.Array)
		if !ok {
			return "GeometryCollection geometries must be an array"
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
	return coordinatesProblem(name, getOrNull(doc, "coordinates"), depth)
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
	case typ == "LineString" && depth == 1, typ == "MultiLineString" && depth == 1:
		if arr.Len() < 2 {
			return "GeoJSON LineString must have at least 2 vertices: " + types.FormatAnyValue(arr)
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

func ringProblem(ring *types.Array) string {
	if ring.Len() < 4 {
		return "Loop must have at least 3 different vertices: " + types.FormatAnyValue(ring)
	}
	first, last := must.NotFail(ring.Get(0)), must.NotFail(ring.Get(ring.Len()-1))
	if types.Compare(first, last) != types.Equal {
		return "Loop is not closed, first vertex does not equal last vertex: " + types.FormatAnyValue(ring)
	}
	var distinct []any
	for i := range ring.Len() - 1 {
		v := must.NotFail(ring.Get(i))
		seen := false
		for _, d := range distinct {
			if types.Compare(d, v) == types.Equal {
				seen = true
				break
			}
		}
		if !seen {
			distinct = append(distinct, v)
		}
	}
	if len(distinct) < 3 {
		return "Loop must have at least 3 different vertices: " + types.FormatAnyValue(ring)
	}
	return ""
}

func positionProblem(v any) string {
	arr, ok := v.(*types.Array)
	if !ok || arr.Len() < 2 {
		return "GeoJSON coordinates must be an array of at least two numbers"
	}
	a, b := must.NotFail(arr.Get(0)), must.NotFail(arr.Get(1))
	if _, isNumber := numericType(a); !isNumber {
		return "GeoJSON coordinates must be numbers"
	}
	if _, isNumber := numericType(b); !isNumber {
		return "GeoJSON coordinates must be numbers"
	}
	x, y, ok := numberPair(a, b)
	if !ok {
		return "Point coordinates must be finite numbers"
	}
	if x < -180 || x > 180 || y < -90 || y > 90 {
		return fmt.Sprintf("Longitude/latitude is out of bounds, lng: %g lat: %g", x, y)
	}
	return ""
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
