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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// circle returns n distinct points on a circle, closed when closed is set.
func circle(n int, closed bool) *types.Array {
	points := types.MakeArray(n + 1)
	for i := range n {
		angle := 2 * math.Pi * float64(i) / float64(n)
		points.Append(must.NotFail(types.NewArray(10*math.Cos(angle), 10*math.Sin(angle))))
	}
	if closed {
		points.Append(must.NotFail(points.Get(0)))
	}
	return points
}

// Large shapes are checked in time linear in their vertices: a quadratic
// distinct vertex count takes hours on these.
func TestSphereIndexProblemLargeShapes(t *testing.T) {
	const vertices = 500_000
	for name, shape := range map[string]*types.Document{
		"LineString": must.NotFail(types.NewDocument("type", "LineString", "coordinates", circle(vertices, false))),
		"Polygon": must.NotFail(types.NewDocument("type", "Polygon",
			"coordinates", must.NotFail(types.NewArray(circle(vertices, true))))),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			require.Empty(t, SphereIndexProblem("loc", shape))
			require.Less(t, time.Since(start), 30*time.Second)
		})
	}
}
