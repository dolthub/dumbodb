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

package stages

import (
	"context"
	"fmt"
	"math"
	stdsort "sort"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// geoNear implements the $geoNear aggregation stage.
//
// Supported fields:
//
//	near                -- the query point; GeoJSON Point or [lon,lat] array
//	distanceField       -- output field that receives the computed distance
//	spherical           -- if true (or field is 2dsphere), use Haversine distance
//	maxDistance         -- maximum distance in metres (spherical) or degrees (planar)
//	minDistance         -- minimum distance in metres (spherical) or degrees (planar)
//	query               -- additional filter document applied before distance check
//	key                 -- field name to use for distance computation (optional)
//	includeLocs         -- output field that receives the matched location value
//	distanceMultiplier  -- scalar multiplier applied to the computed distance
type geoNear struct {
	nearLon            float64
	nearLat            float64
	spherical          bool
	distanceField      string
	maxDistance        float64
	minDistance        float64
	query              *types.Document // optional additional filter
	key                string          // geo field name (optional  -- auto-detected)
	includeLocs        string          // optional output field for matched location
	distanceMultiplier float64         // multiplier applied to distance (default 1)
}

// newGeoNear creates a new $geoNear stage.
func newGeoNear(stage *types.Document) (aggregations.Stage, error) {
	spec, err := common.GetRequiredParam[*types.Document](stage, "$geoNear")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$geoNear requires a document argument",
			"$geoNear (stage)",
		)
	}

	g := &geoNear{
		maxDistance:        math.MaxFloat64,
		distanceMultiplier: 1.0,
	}

	// Parse "near" field
	nearAny, err := spec.Get("near")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$geoNear requires 'near' field",
			"$geoNear (stage)",
		)
	}

	switch nv := nearAny.(type) {
	case *types.Document:
		// GeoJSON Point
		lon, lat, err := extractGeoJSONPointCoords(nv)
		if err != nil {
			return nil, err
		}
		g.nearLon, g.nearLat = lon, lat
	case *types.Array:
		// Legacy [lon, lat]
		if nv.Len() < 2 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'near' array must have at least 2 elements",
				"$geoNear (stage)",
			)
		}
		lon, err := geoToFloat64(must.NotFail(nv.Get(0)))
		if err != nil {
			return nil, err
		}
		lat, err := geoToFloat64(must.NotFail(nv.Get(1)))
		if err != nil {
			return nil, err
		}
		g.nearLon, g.nearLat = lon, lat
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$geoNear 'near' must be a GeoJSON Point or [lon,lat] array",
			"$geoNear (stage)",
		)
	}

	// Parse "distanceField"
	if dfAny, e := spec.Get("distanceField"); e == nil {
		df, ok := dfAny.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'distanceField' must be a string",
				"$geoNear (stage)",
			)
		}
		g.distanceField = df
	}

	// Parse "spherical"
	if sphAny, e := spec.Get("spherical"); e == nil {
		sph, ok := sphAny.(bool)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'spherical' must be a boolean",
				"$geoNear (stage)",
			)
		}
		g.spherical = sph
	}

	// Parse "maxDistance"
	if maxAny, e := spec.Get("maxDistance"); e == nil {
		maxD, err := geoToFloat64(maxAny)
		if err != nil {
			return nil, err
		}
		g.maxDistance = maxD
	}

	// Parse "minDistance"
	if minAny, e := spec.Get("minDistance"); e == nil {
		minD, err := geoToFloat64(minAny)
		if err != nil {
			return nil, err
		}
		g.minDistance = minD
	}

	// Parse "query" (additional filter)
	if qAny, e := spec.Get("query"); e == nil {
		qDoc, ok := qAny.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'query' must be a document",
				"$geoNear (stage)",
			)
		}
		g.query = qDoc
	}

	// Parse "key" (geo field override)
	if keyAny, e := spec.Get("key"); e == nil {
		k, ok := keyAny.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'key' must be a string",
				"$geoNear (stage)",
			)
		}
		g.key = k
	}

	// Parse "includeLocs" (optional field name for the matched location)
	if ilAny, e := spec.Get("includeLocs"); e == nil {
		il, ok := ilAny.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$geoNear 'includeLocs' must be a string",
				"$geoNear (stage)",
			)
		}
		g.includeLocs = il
	}

	// Parse "distanceMultiplier"
	if dmAny, e := spec.Get("distanceMultiplier"); e == nil {
		dm, err := geoToFloat64(dmAny)
		if err != nil {
			return nil, err
		}
		g.distanceMultiplier = dm
	}

	// Validate near coordinates when spherical
	if g.spherical {
		if g.nearLon < -180 || g.nearLon > 180 || g.nearLat < -90 || g.nearLat > 90 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"invalid argument in geo near query: type",
				"$geoNear (stage)",
			)
		}
	}

	return g, nil
}

// Process implements the aggregations.Stage interface.
func (g *geoNear) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) {
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, err
	}

	type docWithDist struct {
		doc  *types.Document
		dist float64
	}

	var results []docWithDist

	for _, doc := range docs {
		// Apply optional query filter.
		if g.query != nil {
			match, err := common.FilterDocument(doc, g.query)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
		}

		// Find the geo field.
		geoField := g.key
		if geoField == "" {
			geoField = g.autoDetectGeoField(doc)
		}
		if geoField == "" {
			continue
		}

		fieldVal, e := doc.Get(geoField)
		if e != nil {
			continue
		}

		dist := g.computeDistance(fieldVal)
		if dist < g.minDistance || dist > g.maxDistance {
			continue
		}

		// Clone the doc and add the distanceField.
		out := doc.DeepCopy()
		scaledDist := dist * g.distanceMultiplier
		if g.distanceField != "" {
			out.Set(g.distanceField, scaledDist)
		}
		if g.includeLocs != "" {
			// Set includeLocs to the value of the geo field.
			if locVal, e := doc.Get(geoField); e == nil {
				out.Set(g.includeLocs, locVal)
			}
		}

		results = append(results, docWithDist{doc: out, dist: dist})
	}

	// Sort ascending by distance.
	stdsort.SliceStable(results, func(i, j int) bool {
		return results[i].dist < results[j].dist
	})

	sorted := make([]*types.Document, len(results))
	for i, r := range results {
		sorted[i] = r.doc
	}

	res := iterator.Values(iterator.ForSlice(sorted))
	closer.Add(res)
	return res, nil
}

// autoDetectGeoField returns the first field that looks like a GeoJSON geometry or
// legacy coordinate pair.  Returns "" if none found.
func (g *geoNear) autoDetectGeoField(doc *types.Document) string {
	iter := doc.Iterator()
	defer iter.Close()

	for {
		field, val, err := iter.Next()
		if err != nil {
			break
		}
		if field == "_id" {
			continue
		}
		switch v := val.(type) {
		case *types.Document:
			if t, e := v.Get("type"); e == nil {
				if ts, ok := t.(string); ok {
					switch ts {
					case "Point", "LineString", "Polygon",
						"MultiPoint", "MultiLineString", "MultiPolygon", "GeometryCollection":
						return field
					}
				}
			}
		case *types.Array:
			if v.Len() >= 2 {
				return field
			}
		}
	}
	return ""
}

// computeDistance returns the distance from the query point to fieldVal.
func (g *geoNear) computeDistance(fieldVal any) float64 {
	switch v := fieldVal.(type) {
	case *types.Document:
		lon, lat, err := extractGeoJSONPointCoords(v)
		if err != nil {
			return math.MaxFloat64
		}
		if g.spherical {
			return haversineDistMeters(g.nearLon, g.nearLat, lon, lat)
		}
		return euclideanDist(g.nearLon, g.nearLat, lon, lat)
	case *types.Array:
		if v.Len() < 2 {
			return math.MaxFloat64
		}
		lon, err := geoToFloat64(must.NotFail(v.Get(0)))
		if err != nil {
			return math.MaxFloat64
		}
		lat, err := geoToFloat64(must.NotFail(v.Get(1)))
		if err != nil {
			return math.MaxFloat64
		}
		if g.spherical {
			return haversineDistMeters(g.nearLon, g.nearLat, lon, lat)
		}
		return euclideanDist(g.nearLon, g.nearLat, lon, lat)
	}
	return math.MaxFloat64
}

// ----------------------------------------------
// Local helpers (avoid import cycle with common)
// ----------------------------------------------

const earthRadiusM = 6378137.0

func haversineDistMeters(lon1, lat1, lon2, lat2 float64) float64 {
	const pi180 = math.Pi / 180
	dLat := (lat2 - lat1) * pi180
	dLon := (lon2 - lon1) * pi180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*pi180)*math.Cos(lat2*pi180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusM * c
}

func euclideanDist(x1, y1, x2, y2 float64) float64 {
	dx := x2 - x1
	dy := y2 - y1
	return math.Sqrt(dx*dx + dy*dy)
}

func extractGeoJSONPointCoords(doc *types.Document) (float64, float64, error) {
	coordsAny, err := doc.Get("coordinates")
	if err != nil {
		return 0, 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "GeoJSON Point missing 'coordinates'", "$geoNear",
		)
	}
	arr, ok := coordsAny.(*types.Array)
	if !ok || arr.Len() < 2 {
		return 0, 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "GeoJSON Point coordinates must be [lon, lat]", "$geoNear",
		)
	}
	lon, err := geoToFloat64(must.NotFail(arr.Get(0)))
	if err != nil {
		return 0, 0, err
	}
	lat, err := geoToFloat64(must.NotFail(arr.Get(1)))
	if err != nil {
		return 0, 0, err
	}
	return lon, lat, nil
}

func geoToFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrBadValue,
		fmt.Sprintf("$geoNear: expected numeric value, got %T", v),
		"$geoNear",
	)
}

// check interface
var _ aggregations.Stage = (*geoNear)(nil)
