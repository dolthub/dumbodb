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
	"strings"
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

// A refused value is echoed in full up to echoLimit, and cut short above it,
// however large it is.
func TestSphereIndexProblemEchoIsBounded(t *testing.T) {
	pad := strings.Repeat("x", 15<<20)
	for name, v := range map[string]any{
		"legacy point":    must.NotFail(types.NewArray(int32(500), int32(0), pad)),
		"legacy document": must.NotFail(types.NewDocument("x", int32(500), "y", int32(0), "z", pad)),
		"open polygon": must.NotFail(types.NewDocument("type", "Polygon",
			"coordinates", must.NotFail(types.NewArray(circle(100_000, false))))),
		"unknown type": must.NotFail(types.NewDocument("type", "Foo", "pad", pad)),
	} {
		t.Run(name, func(t *testing.T) {
			problem := SphereIndexProblem("loc", v)
			require.NotEmpty(t, problem)
			require.Less(t, len(problem), echoLimit+1024)
		})
	}
}

func TestEchoThreshold(t *testing.T) {
	realistic := circle(5_000, false)
	require.Equal(t, types.FormatAnyValue(realistic), Echo(realistic))
	require.False(t, strings.HasSuffix(Echo(realistic), "..."))

	for n := 1; ; n *= 2 {
		points := circle(n, false)
		full := types.FormatAnyValue(points)
		if len(full) <= echoLimit {
			require.Equal(t, full, Echo(points))
			continue
		}
		echoed := Echo(points)
		require.True(t, strings.HasSuffix(echoed, "..."))
		require.Equal(t, full[:echoLimit], strings.TrimSuffix(echoed, "..."))
		break
	}
}
