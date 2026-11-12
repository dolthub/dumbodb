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
	"fmt"
	"math"
	"slices"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// percentileAccumulator implements $percentile and $median with mongod's
// only method, "approximate": the percentile p of n numeric values is the
// value at rank ceil(p*n).
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
		spec, ok := args[0].(*types.Document)
		if !ok {
			return nil, specNotObjectError(notObject, name, args[0])
		}
		var input, p, method any
		for _, key := range spec.Keys() {
			v := must.NotFail(spec.Get(key))
			switch {
			case key == "input":
				input = v
			case key == "p" && !median:
				p = v
			case key == "method":
				method = v
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field '%s.%s' is an unknown field.", name, key), name,
				)
			}
		}
		missing := func(field string) error {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrMissingField, fmt.Sprintf("BSON field '%s.%s' is missing but a required field", name, field), name,
			)
		}
		switch {
		case input == nil:
			return nil, missing("input")
		case p == nil && !median:
			return nil, missing("p")
		case method == nil:
			return nil, missing("method")
		}
		if _, ok := method.(string); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field '%s.method' is the wrong type '%s', expected type 'string'", name, handlerparams.AliasFromType(method)),
				name,
			)
		}
		if method != "approximate" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue, "Currently only 'approximate' can be used as percentile 'method'.", name,
			)
		}

		acc := &percentileAccumulator{median: median, ps: []float64{0.5}}
		var err error
		if acc.input, err = newOperand(input); err != nil {
			return nil, err
		}
		if !median {
			if acc.ps, err = parsePercentiles(p); err != nil {
				return nil, err
			}
		}
		return acc, nil
	}
}

// parsePercentiles returns the constant percentiles of p.
func parsePercentiles(p any) ([]float64, error) {
	if !operators.IsConstantExpression(p) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrPercentileNotConstant,
			"The $percentile 'p' field must be an array of constant values, but found value: "+types.FormatAnyValue(p)+".",
			"$percentile",
		)
	}
	v, _, err := operators.EvalOperand(p, types.MakeDocument(0))
	if err != nil {
		return nil, err
	}
	invalid := func(code handlererrors.ErrorCode, found any) error {
		return handlererrors.NewCommandErrorMsgWithArgument(
			code,
			"The $percentile 'p' field must be an array of numbers from [0.0, 1.0], but found: "+types.FormatAnyValue(found),
			"$percentile",
		)
	}
	arr, ok := v.(*types.Array)
	if !ok || arr.Len() == 0 {
		return nil, invalid(handlererrors.ErrPercentileNotArray, v)
	}
	ps := make([]float64, 0, arr.Len())
	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))
		f, ok := numberAsFloat(elem)
		if !ok {
			return nil, invalid(handlererrors.ErrPercentileNotNumber, elem)
		}
		if f < 0 || f > 1 || math.IsNaN(f) {
			return nil, invalid(handlererrors.ErrPercentileOutOfRange, elem)
		}
		ps = append(ps, f)
	}
	return ps, nil
}

func numberAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case types.Decimal128:
		f, err := aggregations.DecimalToAPD(n).Float64()
		return f, err == nil
	}
	return 0, false
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
	if f, ok := numberAsFloat(v); ok && !math.IsNaN(f) {
		s.nums = append(s.nums, f)
	}
	return nil
}

func (s *percentileState) Result() (any, error) {
	slices.Sort(s.nums)
	at := func(p float64) any {
		if len(s.nums) == 0 {
			return types.Null
		}
		i := int(math.Ceil(p*float64(len(s.nums)))) - 1
		return s.nums[max(0, min(i, len(s.nums)-1))]
	}
	if s.spec.median {
		return at(0.5), nil
	}
	out := types.MakeArray(len(s.spec.ps))
	for _, p := range s.spec.ps {
		out.Append(at(p))
	}
	return out, nil
}

var (
	_ Accumulator  = (*percentileAccumulator)(nil)
	_ Accumulation = (*percentileState)(nil)
)
