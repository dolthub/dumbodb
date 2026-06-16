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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type stdDevPopAccumulator struct {
	expression *aggregations.Expression
	number     any
}

func newStdDevPop(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $stdDevPop accumulator is a unary operator",
			"$stdDevPop (accumulator)",
		)
	}

	accumulator := new(stdDevPopAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.number = int32(0)
		}
	case float64, int32, int64:
		accumulator.number = arg
	default:
		accumulator.number = int32(0)
	}

	return accumulator, nil
}

func (s *stdDevPopAccumulator) New() Accumulation {
	return &stdDevState{expression: s.expression, number: s.number, sample: false}
}

type stdDevSampAccumulator struct {
	expression *aggregations.Expression
	number     any
}

func newStdDevSamp(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $stdDevSamp accumulator is a unary operator",
			"$stdDevSamp (accumulator)",
		)
	}

	accumulator := new(stdDevSampAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.number = int32(0)
		}
	case float64, int32, int64:
		accumulator.number = arg
	default:
		accumulator.number = int32(0)
	}

	return accumulator, nil
}

func (s *stdDevSampAccumulator) New() Accumulation {
	return &stdDevState{expression: s.expression, number: s.number, sample: true}
}

// stdDevState collects numeric values for a group and computes the standard
// deviation at the end. The two-pass mean/variance computation is retained for
// numerical parity, so this accumulator holds the group's numeric values (not
// its documents). A numerically-stable online variant is a future optimization.
type stdDevState struct {
	expression *aggregations.Expression
	number     any
	sample     bool
	nums       []float64
}

func (s *stdDevState) Accumulate(doc *types.Document) error {
	var val any

	if s.expression != nil {
		v, evalErr := s.expression.Evaluate(doc)
		if evalErr != nil {
			return nil
		}

		val = v
	} else {
		val = s.number
	}

	switch v := val.(type) {
	case float64:
		s.nums = append(s.nums, v)
	case int32:
		s.nums = append(s.nums, float64(v))
	case int64:
		s.nums = append(s.nums, float64(v))
	}

	return nil
}

func (s *stdDevState) Result() (any, error) {
	if s.sample {
		if len(s.nums) < 2 {
			return types.Null, nil
		}

		return stdDevSamp(s.nums), nil
	}

	if len(s.nums) == 0 {
		return types.Null, nil
	}

	return stdDevPop(s.nums), nil
}

// stdDevPop computes population standard deviation.
func stdDevPop(nums []float64) float64 {
	n := float64(len(nums))
	mean := 0.0

	for _, v := range nums {
		mean += v
	}

	mean /= n

	variance := 0.0
	for _, v := range nums {
		d := v - mean
		variance += d * d
	}

	variance /= n

	return math.Sqrt(variance)
}

// stdDevSamp computes sample standard deviation.
func stdDevSamp(nums []float64) float64 {
	n := float64(len(nums))
	mean := 0.0

	for _, v := range nums {
		mean += v
	}

	mean /= n

	variance := 0.0
	for _, v := range nums {
		d := v - mean
		variance += d * d
	}

	variance /= n - 1

	return math.Sqrt(variance)
}

var (
	_ Accumulator  = (*stdDevPopAccumulator)(nil)
	_ Accumulator  = (*stdDevSampAccumulator)(nil)
	_ Accumulation = (*stdDevState)(nil)
)
