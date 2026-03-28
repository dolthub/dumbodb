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
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// earthRadiusMeters is the mean radius of the Earth in metres (WGS84 approximation).
const earthRadiusMeters = 6378137.0

// GeoSortKey holds the parameters needed to sort documents by geo distance.
type GeoSortKey struct {
	Field     string  // document field that contains the geometry
	Lon       float64 // query-point longitude
	Lat       float64 // query-point latitude
	Spherical bool    // true ⇒ haversine; false ⇒ Euclidean (2d index)
}

// FindGeoSortKey scans a filter document for a top-level $near or $nearSphere
// operator and, if found, returns the parameters needed for distance-based sorting.
// Returns nil when no geo sort is required.
func FindGeoSortKey(filter *types.Document) *GeoSortKey {
	iter := filter.Iterator()
	defer iter.Close()

	for {
		field, val, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			break
		}
		if field == "" || field[0] == '$' {
			continue
		}

		expr, ok := val.(*types.Document)
		if !ok {
			continue
		}

		for _, opKey := range expr.Keys() {
			if opKey != "$near" && opKey != "$nearSphere" {
				continue
			}

			opVal := must.NotFail(expr.Get(opKey))
			opDoc, ok := opVal.(*types.Document)
			if !ok {
				// legacy 2d: value is an array [lon, lat]
				arr, ok2 := opVal.(*types.Array)
				if !ok2 || arr.Len() < 2 {
					continue
				}
				lon, err := toFloat64(must.NotFail(arr.Get(0)))
				if err != nil {
					continue
				}
				lat, err := toFloat64(must.NotFail(arr.Get(1)))
				if err != nil {
					continue
				}
				return &GeoSortKey{
					Field:     field,
					Lon:       lon,
					Lat:       lat,
					Spherical: opKey == "$nearSphere",
				}
			}

			// $geometry form
			geomAny, err2 := opDoc.Get("$geometry")
			if err2 != nil {
				continue
			}
			geomDoc, ok := geomAny.(*types.Document)
			if !ok {
				continue
			}
			lon, lat, err2 := extractPointCoords(geomDoc)
			if err2 != nil {
				continue
			}
			return &GeoSortKey{
				Field:     field,
				Lon:       lon,
				Lat:       lat,
				Spherical: true,
			}
		}
	}
	return nil
}

// ValidateGeoFilter walks a filter document and validates any $near / $nearSphere
// operators, returning an error if any contain invalid coordinates.
// This is called at query-parse time so that invalid queries are rejected before
// any document scanning takes place (matching MongoDB server behaviour).
func ValidateGeoFilter(filter *types.Document) error {
	iter := filter.Iterator()
	defer iter.Close()

	for {
		field, val, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			break
		}
		if field == "" || field[0] == '$' {
			continue
		}

		expr, ok := val.(*types.Document)
		if !ok {
			continue
		}

		for _, opKey := range expr.Keys() {
			if opKey != "$near" && opKey != "$nearSphere" {
				continue
			}
			opVal := must.NotFail(expr.Get(opKey))
			opDoc, ok := opVal.(*types.Document)
			if !ok {
				continue
			}
			geomAny, err2 := opDoc.Get("$geometry")
			if err2 != nil {
				continue
			}
			geomDoc, ok := geomAny.(*types.Document)
			if !ok {
				continue
			}
			lon, lat, err2 := extractPointCoords(geomDoc)
			if err2 != nil {
				continue
			}
			if lon < -180 || lon > 180 {
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf("longitude must be in [-180, 180], got %v", lon),
					opKey,
				)
			}
			if lat < -90 || lat > 90 {
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf("latitude must be in [-90, 90], got %v", lat),
					opKey,
				)
			}
		}
	}
	return nil
}

// GeoDistanceSortIterator consumes the iterator, computes the distance from the
// geo query point to each document's geometry field, sorts ascending by distance,
// and returns a new iterator over the sorted slice.
func GeoDistanceSortIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, gsk *GeoSortKey) (types.DocumentsIterator, error) {
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	type docDist struct {
		doc  *types.Document
		dist float64
	}

	pairs := make([]docDist, len(docs))
	for i, doc := range docs {
		d := docDistanceFromField(doc, gsk.Field, gsk.Lon, gsk.Lat, gsk.Spherical)
		pairs[i] = docDist{doc: doc, dist: d}
	}

	sort.SliceStable(pairs, func(i, j int) bool {
		return pairs[i].dist < pairs[j].dist
	})

	sorted := make([]*types.Document, len(pairs))
	for i, p := range pairs {
		sorted[i] = p.doc
	}

	res := iterator.Values(iterator.ForSlice(sorted))
	closer.Add(res)
	return res, nil
}

// docDistanceFromField returns the distance from (lon, lat) to the geometry stored
// in the named field of doc. Returns math.MaxFloat64 if the field is missing or
// cannot be interpreted as a Point.
func docDistanceFromField(doc *types.Document, field string, lon, lat float64, spherical bool) float64 {
	val, err := doc.Get(field)
	if err != nil {
		return math.MaxFloat64
	}
	switch v := val.(type) {
	case *types.Document:
		dlon, dlat, err := extractPointCoords(v)
		if err != nil {
			return math.MaxFloat64
		}
		if spherical {
			return haversineMeters(lon, lat, dlon, dlat)
		}
		return euclidean(lon, lat, dlon, dlat)
	case *types.Array:
		if v.Len() < 2 {
			return math.MaxFloat64
		}
		dlon, err := toFloat64(must.NotFail(v.Get(0)))
		if err != nil {
			return math.MaxFloat64
		}
		dlat, err := toFloat64(must.NotFail(v.Get(1)))
		if err != nil {
			return math.MaxFloat64
		}
		if spherical {
			return haversineMeters(lon, lat, dlon, dlat)
		}
		return euclidean(lon, lat, dlon, dlat)
	}
	return math.MaxFloat64
}

// ──────────────────────────────────────────────
// Filter operators
// ──────────────────────────────────────────────

// filterFieldGeoWithin handles {field: {$geoWithin: spec}}.
func filterFieldGeoWithin(fieldValue any, spec *types.Document) (bool, error) {
	for _, k := range spec.Keys() {
		v := must.NotFail(spec.Get(k))
		switch k {
		case "$geometry":
			geomDoc, ok := v.(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "$geometry must be a document", "$geoWithin",
				)
			}
			return geometryWithinGeometry(fieldValue, geomDoc)

		case "$box":
			arr, ok := v.(*types.Array)
			if !ok || arr.Len() < 2 {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "$box must be [[x1,y1],[x2,y2]]", "$geoWithin",
				)
			}
			bl, err := arrToPoint(must.NotFail(arr.Get(0)))
			if err != nil {
				return false, err
			}
			tr, err := arrToPoint(must.NotFail(arr.Get(1)))
			if err != nil {
				return false, err
			}
			px, py, err := fieldPoint(fieldValue)
			if err != nil {
				return false, nil // non-point field, not within
			}
			minX, maxX := math.Min(bl[0], tr[0]), math.Max(bl[0], tr[0])
			minY, maxY := math.Min(bl[1], tr[1]), math.Max(bl[1], tr[1])
			return px >= minX && px <= maxX && py >= minY && py <= maxY, nil

		case "$center":
			arr, ok := v.(*types.Array)
			if !ok || arr.Len() < 2 {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "$center must be [[cx,cy], radius]", "$geoWithin",
				)
			}
			center, err := arrToPoint(must.NotFail(arr.Get(0)))
			if err != nil {
				return false, err
			}
			radius, err := toFloat64(must.NotFail(arr.Get(1)))
			if err != nil {
				return false, err
			}
			px, py, err := fieldPoint(fieldValue)
			if err != nil {
				return false, nil
			}
			dist := euclidean(center[0], center[1], px, py)
			return dist <= radius, nil

		case "$centerSphere":
			arr, ok := v.(*types.Array)
			if !ok || arr.Len() < 2 {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "$centerSphere must be [[lon,lat], radiusRadians]", "$geoWithin",
				)
			}
			center, err := arrToPoint(must.NotFail(arr.Get(0)))
			if err != nil {
				return false, err
			}
			radiusRad, err := toFloat64(must.NotFail(arr.Get(1)))
			if err != nil {
				return false, err
			}
			radiusMeters := radiusRad * earthRadiusMeters
			px, py, err := fieldPoint(fieldValue)
			if err != nil {
				return false, nil
			}
			dist := haversineMeters(center[0], center[1], px, py)
			return dist <= radiusMeters, nil

		case "$polygon":
			arr, ok := v.(*types.Array)
			if !ok {
				return false, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "$polygon must be an array of points", "$geoWithin",
				)
			}
			ring, err := arrToRing(arr)
			if err != nil {
				return false, err
			}
			px, py, err := fieldPoint(fieldValue)
			if err != nil {
				return false, nil
			}
			return pointInRing(px, py, ring), nil

		default:
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("unknown $geoWithin shape: %s", k),
				"$geoWithin",
			)
		}
	}
	return false, nil
}

// filterFieldGeoIntersects handles {field: {$geoIntersects: {$geometry: ...}}}.
func filterFieldGeoIntersects(fieldValue any, spec *types.Document) (bool, error) {
	geomAny, err := spec.Get("$geometry")
	if err != nil {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "$geoIntersects requires $geometry", "$geoIntersects",
		)
	}
	queryGeomDoc, ok := geomAny.(*types.Document)
	if !ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "$geometry must be a document", "$geoIntersects",
		)
	}
	return geometryIntersectsGeometry(fieldValue, queryGeomDoc)
}

// filterFieldNear handles {field: {$near: ...}} and {field: {$nearSphere: ...}}.
// It filters by distance bounds (maxDistance / minDistance).
// Sorting by distance is handled separately at the cursor level.
func filterFieldNear(fieldValue any, opDoc *types.Document, spherical bool) (bool, error) {
	var queryLon, queryLat float64
	var maxDist, minDist float64 = math.MaxFloat64, 0

	// Determine if it's the $geometry form or legacy array form.
	if geomAny, err := opDoc.Get("$geometry"); err == nil {
		geomDoc, ok := geomAny.(*types.Document)
		if !ok {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue, "$geometry must be a document", "$near",
			)
		}
		lon, lat, err := extractPointCoords(geomDoc)
		if err != nil {
			return false, err
		}
		// Validate coordinate ranges
		if lon < -180 || lon > 180 {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("longitude must be in [-180, 180], got %v", lon),
				"$near",
			)
		}
		if lat < -90 || lat > 90 {
			return false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("latitude must be in [-90, 90], got %v", lat),
				"$near",
			)
		}
		queryLon, queryLat = lon, lat
		spherical = true // $geometry always spherical
	}

	if maxAny, err := opDoc.Get("$maxDistance"); err == nil {
		maxDist, err = toFloat64(maxAny)
		if err != nil {
			return false, err
		}
	}

	if minAny, err := opDoc.Get("$minDistance"); err == nil {
		minDist, err = toFloat64(minAny)
		if err != nil {
			return false, err
		}
	}

	// Handle legacy array form: {$near: [lon, lat], $maxDistance: n}
	// In that case opDoc won't have $geometry but also won't have $geometry key at all —
	// the entire opDoc IS what $near points to.  If it has $geometry it was handled above.
	// If it doesn't have $geometry, it must be a legacy coord array embedded as if a doc
	// with no $geometry.  But legacy $near looks like:
	//   { coords: { $near: [x, y], $maxDistance: 1 } }
	// Here the value of $near is an *Array, not a *Document, so filterFieldExpr would
	// pass the array to filterFieldNear.  Actually we receive the $near value document here
	// so this case is handled in filterFieldExpr by detecting array vs document.
	// If we reach here without $geometry, we assume the coordinates were extracted in
	// the caller for legacy 2d form and the distance units are Euclidean degrees.

	// Compute distance from query point to field value.
	var dist float64
	switch v := fieldValue.(type) {
	case *types.Document:
		dlon, dlat, err := extractPointCoords(v)
		if err != nil {
			return false, nil // non-point geometry — skip
		}
		if spherical {
			dist = haversineMeters(queryLon, queryLat, dlon, dlat)
		} else {
			dist = euclidean(queryLon, queryLat, dlon, dlat)
		}
	case *types.Array:
		if v.Len() < 2 {
			return false, nil
		}
		dlon, err := toFloat64(must.NotFail(v.Get(0)))
		if err != nil {
			return false, nil
		}
		dlat, err := toFloat64(must.NotFail(v.Get(1)))
		if err != nil {
			return false, nil
		}
		if spherical {
			dist = haversineMeters(queryLon, queryLat, dlon, dlat)
		} else {
			dist = euclidean(queryLon, queryLat, dlon, dlat)
		}
	default:
		return false, nil
	}

	return dist >= minDist && dist <= maxDist, nil
}

// ──────────────────────────────────────────────
// Geometry containment / intersection
// ──────────────────────────────────────────────

// geometryWithinGeometry reports whether docGeom (from the document) is entirely
// within queryGeom.
func geometryWithinGeometry(docGeomVal any, queryGeomDoc *types.Document) (bool, error) {
	queryType, err := geoJSONType(queryGeomDoc)
	if err != nil {
		return false, err
	}

	switch queryType {
	case "Polygon":
		rings, err := polygonRings(queryGeomDoc)
		if err != nil {
			return false, err
		}
		exterior := rings[0]
		return docGeomWithinPolygon(docGeomVal, exterior), nil

	case "MultiPolygon":
		polys, err := multiPolygonRings(queryGeomDoc)
		if err != nil {
			return false, err
		}
		for _, rings := range polys {
			if docGeomWithinPolygon(docGeomVal, rings[0]) {
				return true, nil
			}
		}
		return false, nil

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("$geoWithin $geometry type %q not implemented", queryType),
			"$geoWithin",
		)
	}
}

// docGeomWithinPolygon returns true if all points of the document geometry lie
// within the given exterior ring (uses even-odd ray casting).
func docGeomWithinPolygon(docGeomVal any, exterior [][2]float64) bool {
	pts := extractAllPoints(docGeomVal)
	if len(pts) == 0 {
		return false
	}
	for _, p := range pts {
		if !pointInRing(p[0], p[1], exterior) {
			return false
		}
	}
	return true
}

// geometryIntersectsGeometry reports whether the document geometry intersects the
// query geometry.
func geometryIntersectsGeometry(docGeomVal any, queryGeomDoc *types.Document) (bool, error) {
	queryType, err := geoJSONType(queryGeomDoc)
	if err != nil {
		return false, err
	}

	switch queryType {
	case "Polygon":
		rings, err := polygonRings(queryGeomDoc)
		if err != nil {
			return false, err
		}
		exterior := rings[0]
		return docGeomIntersectsPolygon(docGeomVal, exterior), nil

	case "MultiPolygon":
		polys, err := multiPolygonRings(queryGeomDoc)
		if err != nil {
			return false, err
		}
		for _, rings := range polys {
			if docGeomIntersectsPolygon(docGeomVal, rings[0]) {
				return true, nil
			}
		}
		return false, nil

	case "Point":
		qLon, qLat, err := extractPointCoords(queryGeomDoc)
		if err != nil {
			return false, err
		}
		pts := extractAllPoints(docGeomVal)
		for _, p := range pts {
			if p[0] == qLon && p[1] == qLat {
				return true, nil
			}
		}
		return false, nil

	case "LineString":
		qCoords, err := lineStringCoords(queryGeomDoc)
		if err != nil {
			return false, err
		}
		return docGeomIntersectsLineString(docGeomVal, qCoords), nil

	default:
		return false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("$geoIntersects $geometry type %q not implemented", queryType),
			"$geoIntersects",
		)
	}
}

// docGeomIntersectsPolygon returns true if any part of the document geometry
// intersects the given exterior ring.
func docGeomIntersectsPolygon(docGeomVal any, exterior [][2]float64) bool {
	pts := extractAllPoints(docGeomVal)
	if len(pts) == 0 {
		return false
	}

	// Check if any point is inside the polygon.
	for _, p := range pts {
		if pointInRing(p[0], p[1], exterior) {
			return true
		}
	}

	// For LineString / MultiLineString: check if any segment crosses the polygon boundary.
	switch v := docGeomVal.(type) {
	case *types.Document:
		gtype, _ := geoJSONType(v)
		switch gtype {
		case "LineString":
			coords, err := lineStringCoords(v)
			if err == nil && segmentsCrossRing(coords, exterior) {
				return true
			}
		case "MultiLineString":
			lines, err := multiLineStringCoords(v)
			if err == nil {
				for _, line := range lines {
					if segmentsCrossRing(line, exterior) {
						return true
					}
				}
			}
		}
	}

	return false
}

// docGeomIntersectsLineString returns true if any part of the document geometry
// intersects the given LineString coordinate sequence.
func docGeomIntersectsLineString(docGeomVal any, lineCoords [][2]float64) bool {
	// Check if any vertex of the document geometry lies exactly on the LineString.
	pts := extractAllPoints(docGeomVal)
	for _, p := range pts {
		if pointOnLineString(p, lineCoords) {
			return true
		}
	}

	// For polygon-like document geometries: also check if any query line segment
	// crosses the polygon boundary, or if any query line point is inside the polygon.
	if v, ok := docGeomVal.(*types.Document); ok {
		gtype, _ := geoJSONType(v)
		switch gtype {
		case "Polygon":
			rings, err := polygonRings(v)
			if err == nil && len(rings) > 0 {
				exterior := rings[0]
				for _, qp := range lineCoords {
					if pointInRing(qp[0], qp[1], exterior) {
						return true
					}
				}
				if segmentsCrossRing(lineCoords, exterior) {
					return true
				}
			}
		case "MultiPolygon":
			polys, err := multiPolygonRings(v)
			if err == nil {
				for _, rings := range polys {
					exterior := rings[0]
					for _, qp := range lineCoords {
						if pointInRing(qp[0], qp[1], exterior) {
							return true
						}
					}
					if segmentsCrossRing(lineCoords, exterior) {
						return true
					}
				}
			}
		}
	}

	return false
}

// pointOnLineString returns true if p lies exactly on any segment of the line.
func pointOnLineString(p [2]float64, lineCoords [][2]float64) bool {
	for i := 0; i < len(lineCoords)-1; i++ {
		if pointOnSegment(p, lineCoords[i], lineCoords[i+1]) {
			return true
		}
	}
	return false
}

// pointOnSegment returns true if p lies on the segment from a to b.
func pointOnSegment(p, a, b [2]float64) bool {
	// Check collinearity using cross product.
	cross := (b[0]-a[0])*(p[1]-a[1]) - (b[1]-a[1])*(p[0]-a[0])
	if cross != 0 {
		return false
	}
	// p is collinear — check it falls within the bounding box of [a, b].
	minX, maxX := a[0], b[0]
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	minY, maxY := a[1], b[1]
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	return p[0] >= minX && p[0] <= maxX && p[1] >= minY && p[1] <= maxY
}

// segmentsCrossRing returns true if any segment of lineCoords crosses any edge of ring.
func segmentsCrossRing(lineCoords, ring [][2]float64) bool {
	for i := 0; i < len(lineCoords)-1; i++ {
		a1, a2 := lineCoords[i], lineCoords[i+1]
		for j := 0; j < len(ring)-1; j++ {
			b1, b2 := ring[j], ring[j+1]
			if segmentsIntersect(a1, a2, b1, b2) {
				return true
			}
		}
	}
	return false
}

// segmentsIntersect returns true if segment [a1,a2] and [b1,b2] intersect.
func segmentsIntersect(a1, a2, b1, b2 [2]float64) bool {
	d1x := a2[0] - a1[0]
	d1y := a2[1] - a1[1]
	d2x := b2[0] - b1[0]
	d2y := b2[1] - b1[1]
	cross := d1x*d2y - d1y*d2x
	if cross == 0 {
		return false // parallel
	}
	dx := b1[0] - a1[0]
	dy := b1[1] - a1[1]
	t := (dx*d2y - dy*d2x) / cross
	u := (dx*d1y - dy*d1x) / cross
	return t >= 0 && t <= 1 && u >= 0 && u <= 1
}

// ──────────────────────────────────────────────
// GeoJSON parsing helpers
// ──────────────────────────────────────────────

// geoJSONType returns the "type" field of a GeoJSON document.
func geoJSONType(doc *types.Document) (string, error) {
	typeAny, err := doc.Get("type")
	if err != nil {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "GeoJSON document missing 'type' field", "$geo",
		)
	}
	t, ok := typeAny.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "GeoJSON 'type' must be a string", "$geo",
		)
	}
	return t, nil
}

// extractPointCoords returns (lon, lat) from a GeoJSON Point document.
func extractPointCoords(doc *types.Document) (float64, float64, error) {
	typeAny, _ := doc.Get("type")
	if t, ok := typeAny.(string); ok && t != "Point" {
		return 0, 0, fmt.Errorf("expected GeoJSON Point, got %q", t)
	}
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return 0, 0, fmt.Errorf("Point missing coordinates")
	}
	arr, ok := coordsAny.(*types.Array)
	if !ok || arr.Len() < 2 {
		return 0, 0, fmt.Errorf("Point coordinates must be [lon, lat]")
	}
	lon, err := toFloat64(must.NotFail(arr.Get(0)))
	if err != nil {
		return 0, 0, err
	}
	lat, err := toFloat64(must.NotFail(arr.Get(1)))
	if err != nil {
		return 0, 0, err
	}
	return lon, lat, nil
}

// polygonRings returns the rings of a GeoJSON Polygon.
// rings[0] is the exterior ring; rings[1:] are holes (not yet used for within tests).
func polygonRings(doc *types.Document) ([][][2]float64, error) {
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return nil, fmt.Errorf("Polygon missing coordinates")
	}
	ringsArr, ok := coordsAny.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("Polygon coordinates must be an array of rings")
	}

	rings := make([][][2]float64, ringsArr.Len())
	for i := 0; i < ringsArr.Len(); i++ {
		ringAny := must.NotFail(ringsArr.Get(i))
		ringArr, ok := ringAny.(*types.Array)
		if !ok {
			return nil, fmt.Errorf("Polygon ring must be an array of coordinates")
		}
		ring, err := arrToRing(ringArr)
		if err != nil {
			return nil, err
		}
		rings[i] = ring
	}
	return rings, nil
}

// multiPolygonRings returns the rings of a GeoJSON MultiPolygon.
func multiPolygonRings(doc *types.Document) ([][][][2]float64, error) {
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return nil, fmt.Errorf("MultiPolygon missing coordinates")
	}
	polysArr, ok := coordsAny.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("MultiPolygon coordinates must be an array")
	}

	polys := make([][][][2]float64, polysArr.Len())
	for i := 0; i < polysArr.Len(); i++ {
		polyAny := must.NotFail(polysArr.Get(i))
		polyArr, ok := polyAny.(*types.Array)
		if !ok {
			return nil, fmt.Errorf("MultiPolygon polygon must be an array of rings")
		}
		rings := make([][][2]float64, polyArr.Len())
		for j := 0; j < polyArr.Len(); j++ {
			ringAny := must.NotFail(polyArr.Get(j))
			ringArr, ok := ringAny.(*types.Array)
			if !ok {
				return nil, fmt.Errorf("MultiPolygon ring must be an array of coordinates")
			}
			ring, err := arrToRing(ringArr)
			if err != nil {
				return nil, err
			}
			rings[j] = ring
		}
		polys[i] = rings
	}
	return polys, nil
}

// lineStringCoords returns the coordinate sequence of a GeoJSON LineString.
func lineStringCoords(doc *types.Document) ([][2]float64, error) {
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return nil, fmt.Errorf("LineString missing coordinates")
	}
	arr, ok := coordsAny.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("LineString coordinates must be an array")
	}
	return arrToRing(arr)
}

// multiLineStringCoords returns each LineString's coordinates from a GeoJSON MultiLineString.
func multiLineStringCoords(doc *types.Document) ([][][2]float64, error) {
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return nil, fmt.Errorf("MultiLineString missing coordinates")
	}
	outer, ok := coordsAny.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("MultiLineString coordinates must be an array")
	}
	lines := make([][][2]float64, outer.Len())
	for i := 0; i < outer.Len(); i++ {
		lineAny := must.NotFail(outer.Get(i))
		lineArr, ok := lineAny.(*types.Array)
		if !ok {
			return nil, fmt.Errorf("MultiLineString line must be an array")
		}
		coords, err := arrToRing(lineArr)
		if err != nil {
			return nil, err
		}
		lines[i] = coords
	}
	return lines, nil
}

// extractAllPoints returns every [lon, lat] pair from any GeoJSON geometry value.
// It handles Point, LineString, Polygon, Multi*, GeometryCollection and
// legacy [lon, lat] arrays.
func extractAllPoints(val any) [][2]float64 {
	switch v := val.(type) {
	case *types.Document:
		t, err := geoJSONType(v)
		if err != nil {
			return nil
		}
		switch t {
		case "Point":
			lon, lat, err := extractPointCoords(v)
			if err != nil {
				return nil
			}
			return [][2]float64{{lon, lat}}

		case "LineString":
			coords, err := lineStringCoords(v)
			if err != nil {
				return nil
			}
			return coords

		case "Polygon":
			rings, err := polygonRings(v)
			if err != nil || len(rings) == 0 {
				return nil
			}
			return rings[0]

		case "MultiPoint":
			coordsAny, _ := v.Get("coordinates")
			arr, ok := coordsAny.(*types.Array)
			if !ok {
				return nil
			}
			var pts [][2]float64
			for i := 0; i < arr.Len(); i++ {
				p, err := arrToPoint(must.NotFail(arr.Get(i)))
				if err == nil {
					pts = append(pts, p)
				}
			}
			return pts

		case "MultiLineString":
			lines, err := multiLineStringCoords(v)
			if err != nil {
				return nil
			}
			var pts [][2]float64
			for _, l := range lines {
				pts = append(pts, l...)
			}
			return pts

		case "MultiPolygon":
			polys, err := multiPolygonRings(v)
			if err != nil {
				return nil
			}
			var pts [][2]float64
			for _, p := range polys {
				if len(p) > 0 {
					pts = append(pts, p[0]...)
				}
			}
			return pts

		case "GeometryCollection":
			geomsAny, _ := v.Get("geometries")
			arr, ok := geomsAny.(*types.Array)
			if !ok {
				return nil
			}
			var pts [][2]float64
			for i := 0; i < arr.Len(); i++ {
				sub := must.NotFail(arr.Get(i))
				pts = append(pts, extractAllPoints(sub)...)
			}
			return pts
		}

	case *types.Array:
		if v.Len() >= 2 {
			lon, err := toFloat64(must.NotFail(v.Get(0)))
			if err != nil {
				return nil
			}
			lat, err := toFloat64(must.NotFail(v.Get(1)))
			if err != nil {
				return nil
			}
			return [][2]float64{{lon, lat}}
		}
	}
	return nil
}

// ──────────────────────────────────────────────
// Geometric predicates
// ──────────────────────────────────────────────

// pointInRing returns true if the point (px, py) is inside the ring using the
// even-odd (ray casting) algorithm.
func pointInRing(px, py float64, ring [][2]float64) bool {
	n := len(ring)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := ring[i][0], ring[i][1]
		xj, yj := ring[j][0], ring[j][1]
		if ((yi > py) != (yj > py)) && (px < (xj-xi)*(py-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ──────────────────────────────────────────────
// Distance functions
// ──────────────────────────────────────────────

// haversineMeters computes the great-circle distance in metres between two
// [lon, lat] points using the Haversine formula.
func haversineMeters(lon1, lat1, lon2, lat2 float64) float64 {
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}

// euclidean computes the 2D Euclidean distance between two points.
func euclidean(x1, y1, x2, y2 float64) float64 {
	dx := x2 - x1
	dy := y2 - y1
	return math.Sqrt(dx*dx + dy*dy)
}

// ──────────────────────────────────────────────
// Array conversion helpers
// ──────────────────────────────────────────────

// arrToPoint converts a BSON array [x, y] to [2]float64.
func arrToPoint(val any) ([2]float64, error) {
	arr, ok := val.(*types.Array)
	if !ok || arr.Len() < 2 {
		return [2]float64{}, fmt.Errorf("expected [x, y] array")
	}
	x, err := toFloat64(must.NotFail(arr.Get(0)))
	if err != nil {
		return [2]float64{}, err
	}
	y, err := toFloat64(must.NotFail(arr.Get(1)))
	if err != nil {
		return [2]float64{}, err
	}
	return [2]float64{x, y}, nil
}

// arrToRing converts a BSON array of [x,y] arrays to [][2]float64.
func arrToRing(arr *types.Array) ([][2]float64, error) {
	ring := make([][2]float64, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		pt, err := arrToPoint(must.NotFail(arr.Get(i)))
		if err != nil {
			return nil, err
		}
		ring[i] = pt
	}
	return ring, nil
}

// fieldPoint extracts a point (lon, lat) from a document field value.
// Accepts GeoJSON Point doc or legacy [lon, lat] array.
func fieldPoint(val any) (float64, float64, error) {
	switch v := val.(type) {
	case *types.Document:
		lon, lat, err := extractPointCoords(v)
		return lon, lat, err
	case *types.Array:
		if v.Len() < 2 {
			return 0, 0, fmt.Errorf("expected [lon, lat] array")
		}
		lon, err := toFloat64(must.NotFail(v.Get(0)))
		if err != nil {
			return 0, 0, err
		}
		lat, err := toFloat64(must.NotFail(v.Get(1)))
		if err != nil {
			return 0, 0, err
		}
		return lon, lat, nil
	}
	return 0, 0, fmt.Errorf("field is not a Point geometry")
}

// toFloat64 converts various numeric BSON types to float64.
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, fmt.Errorf("expected numeric value, got %T", v)
}
