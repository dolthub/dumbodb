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

// Package geo reads points from stored geo field values.
package geo

import (
	"math"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Point returns the point a stored field value holds: a GeoJSON Point or a
// legacy [x, y] array, read as mongod reads them. Extra elements are ignored.
// Coordinates are finite numbers of any numeric type.
//
// mongod also reads a legacy point from a document's first two fields, x then
// y, whatever their names. Stored documents have sorted keys, which loses that
// order ({lng: 10, lat: 1} is stored as {lat: 1, lng: 10}), so such a document
// is not a point here.
func Point(v any) (x, y float64, ok bool) {
	switch v := v.(type) {
	case *types.Array:
		if v.Len() < 2 {
			return 0, 0, false
		}
		return numberPair(must.NotFail(v.Get(0)), must.NotFail(v.Get(1)))
	case *types.Document:
		if t, err := v.Get("type"); err == nil {
			coords, _ := v.Get("coordinates")
			arr, isArr := coords.(*types.Array)
			if t != "Point" || !isArr || arr.Len() < 2 {
				return 0, 0, false
			}
			return numberPair(must.NotFail(arr.Get(0)), must.NotFail(arr.Get(1)))
		}
	}
	return 0, 0, false
}

func numberPair(a, b any) (float64, float64, bool) {
	x, okX := number(a)
	y, okY := number(b)
	return x, y, okX && okY
}

func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case types.Decimal128:
		parsed, err := strconv.ParseFloat(bson.NewDecimal128(n.H, n.L).String(), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}
