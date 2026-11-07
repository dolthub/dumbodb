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

// Point returns the point a field value holds, as mongod reads one: a GeoJSON
// Point, a legacy [x, y] array, or a legacy document's first two values. Extra
// elements are ignored. Coordinates are finite numbers of any numeric type.
//
// Documents are stored with sorted keys, so a legacy document's "first two
// values" are those of its first two keys in sorted order, not insertion order.
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
		if values := v.Values(); len(values) >= 2 {
			return numberPair(values[0], values[1])
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
