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
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// overflowingLine is a LineString whose edge length overflows float64.
func overflowingLine() *types.Document {
	return must.NotFail(types.NewDocument("type", "LineString", "coordinates", must.NotFail(types.NewArray(
		must.NotFail(types.NewArray(-1e308, 0.0)), must.NotFail(types.NewArray(1e308, 0.0)),
	))))
}

func TestGeoDistanceNonFiniteIsNoLocation(t *testing.T) {
	for _, spherical := range []bool{false, true} {
		if d := GeoDistance(overflowingLine(), 0, 1, spherical); d != math.MaxFloat64 {
			t.Errorf("spherical=%t: distance to the overflowing line = %v, want no location", spherical, d)
		}
		near := must.NotFail(types.NewArray(overflowingLine(), must.NotFail(types.NewArray(0.0, 1.5))))
		d := GeoDistance(near, 0, 1, spherical)
		if math.IsNaN(d) || d == math.MaxFloat64 {
			t.Errorf("spherical=%t: distance beside a near point = %v, want the point's", spherical, d)
		}
	}
}
