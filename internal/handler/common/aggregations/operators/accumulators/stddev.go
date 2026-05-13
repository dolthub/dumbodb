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
	"errors"
	"math"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
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

func (s *stdDevPopAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	nums, err := collectNumericValues(iter, s.expression, s.number)
	if err != nil {
		return nil, err
	}

	if len(nums) == 0 {
		return types.Null, nil
	}

	return stdDevPop(nums), nil
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

func (s *stdDevSampAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	nums, err := collectNumericValues(iter, s.expression, s.number)
	if err != nil {
		return nil, err
	}

	if len(nums) < 2 {
		return types.Null, nil
	}

	return stdDevSamp(nums), nil
}

// collectNumericValues iterates documents, evaluating expression or using the constant,
// and returns only numeric (float64) values.
func collectNumericValues(iter types.DocumentsIterator, expr *aggregations.Expression, constant any) ([]float64, error) {
	var nums []float64

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var val any

		if expr != nil {
			v, evalErr := expr.Evaluate(doc)
			if evalErr != nil {
				continue
			}

			val = v
		} else {
			val = constant
		}

		switch v := val.(type) {
		case float64:
			nums = append(nums, v)
		case int32:
			nums = append(nums, float64(v))
		case int64:
			nums = append(nums, float64(v))
		}
	}

	return nums, nil
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
	_ Accumulator = (*stdDevPopAccumulator)(nil)
	_ Accumulator = (*stdDevSampAccumulator)(nil)
)
