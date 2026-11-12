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

package accumulators

import (
	"math"
	"slices"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// percentileAccumulator implements $percentile and $median.
type percentileAccumulator struct {
	input  operand
	ps     []float64
	median bool
}

func newPercentileFunc(name string, median bool) newAccumulatorFunc {
	notObject := handlererrors.ErrPercentileSpecNotObject
	if median {
		notObject = handlererrors.ErrMedianSpecNotObject
	}
	return func(args ...any) (Accumulator, error) {
		spec, err := operators.ParsePercentileSpec(name, median, args[0], notObject)
		if err != nil {
			return nil, err
		}
		acc := &percentileAccumulator{median: median, ps: spec.Ps}
		if acc.input, err = newOperand(spec.Input); err != nil {
			return nil, err
		}
		return acc, nil
	}
}

func (a *percentileAccumulator) New() Accumulation {
	return &percentileState{spec: a}
}

type percentileState struct {
	spec *percentileAccumulator
	nums []float64
}

func (s *percentileState) Accumulate(doc *types.Document) error {
	v, missing, err := s.spec.input.evaluate(doc)
	if err != nil || missing {
		return err
	}
	if f, ok := operators.NumericValue(v); ok && !math.IsNaN(f) {
		s.nums = append(s.nums, f)
	}
	return nil
}

func (s *percentileState) Result() (any, error) {
	slices.Sort(s.nums)
	if s.spec.median {
		return operators.Percentile(s.nums, 0.5), nil
	}
	out := types.MakeArray(len(s.spec.ps))
	for _, p := range s.spec.ps {
		out.Append(operators.Percentile(s.nums, p))
	}
	return out, nil
}

var (
	_ Accumulator  = (*percentileAccumulator)(nil)
	_ Accumulation = (*percentileState)(nil)
)
