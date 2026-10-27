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

package operators

import (
	"fmt"
	"math"
	"slices"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// accumulatorValues evaluates the operands of an accumulator used as an
// expression: a single operand that evaluates to an array contributes its
// elements, several operands contribute themselves.
func accumulatorValues(args []any, doc *types.Document) ([]any, error) {
	var values []any
	for _, arg := range args {
		var v any
		var err error
		if literal, ok := arg.(*types.Array); ok {
			v, err = EvalArrayExpression(literal, doc)
		} else {
			v, err = evalArgValue(arg, doc)
		}
		if err != nil {
			return nil, err
		}
		if arr, ok := v.(*types.Array); ok && len(args) == 1 {
			for i := 0; i < arr.Len(); i++ {
				values = append(values, must.NotFail(arr.Get(i)))
			}
			continue
		}
		values = append(values, v)
	}
	return values, nil
}

// extremeOp represents { $max: <operands> } and { $min: <operands> }.
type extremeOp struct {
	args []any
	want types.CompareResult
}

func newMaxOp(args ...any) (Operator, error) { return &extremeOp{args: args, want: types.Greater}, nil }

func newMinOp(args ...any) (Operator, error) { return &extremeOp{args: args, want: types.Less}, nil }

// Process returns the greatest (or least) operand in BSON order, ignoring null
// and missing values, or null when none remain.
func (op *extremeOp) Process(doc *types.Document) (any, error) {
	values, err := accumulatorValues(op.args, doc)
	if err != nil {
		return nil, err
	}
	var best any
	for _, v := range values {
		if isNullish(v) {
			continue
		}
		if best == nil || types.CompareOrder(v, best, types.Ascending) == op.want {
			best = v
		}
	}
	if best == nil {
		return types.Null, nil
	}
	return best, nil
}

// stdDevOp represents { $stdDevPop: <operands> } and { $stdDevSamp: <operands> }.
type stdDevOp struct {
	args   []any
	sample bool
}

func newStdDevPopOp(args ...any) (Operator, error) { return &stdDevOp{args: args}, nil }

func newStdDevSampOp(args ...any) (Operator, error) {
	return &stdDevOp{args: args, sample: true}, nil
}

// Process returns the standard deviation of the numeric operands, or null
// when there are none (or, for a sample, fewer than two).
func (op *stdDevOp) Process(doc *types.Document) (any, error) {
	values, err := accumulatorValues(op.args, doc)
	if err != nil {
		return nil, err
	}
	nums := numericValues(values)
	if len(nums) == 0 || (op.sample && len(nums) < 2) {
		return types.Null, nil
	}
	mean := 0.0
	for _, v := range nums {
		mean += v
	}
	mean /= float64(len(nums))
	variance := 0.0
	for _, v := range nums {
		variance += (v - mean) * (v - mean)
	}
	if op.sample {
		variance /= float64(len(nums) - 1)
	} else {
		variance /= float64(len(nums))
	}
	return math.Sqrt(variance), nil
}

func numericValues(values []any) []float64 {
	var nums []float64
	for _, v := range values {
		switch n := v.(type) {
		case int32:
			nums = append(nums, float64(n))
		case int64:
			nums = append(nums, float64(n))
		case float64:
			nums = append(nums, n)
		}
	}
	return nums
}

// nOp represents { $maxN | $minN | $firstN | $lastN: { n: <expr>, input: <expr> } }.
type nOp struct {
	name  string
	n     any
	input any
}

func newNOpFunc(name string) newOperatorFunc {
	return func(args ...any) (Operator, error) {
		spec, ok := singleDocumentArg(args)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("specification must be an object; found %v", argsForMessage(args)),
				name,
			)
		}
		op := &nOp{name: name}
		for _, key := range spec.Keys() {
			switch key {
			case "n":
				op.n = must.NotFail(spec.Get(key))
			case "input":
				op.input = must.NotFail(spec.Get(key))
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFailedToParse,
					fmt.Sprintf("Unknown argument for 'n' operator: %s", key),
					name,
				)
			}
		}
		if op.n == nil || op.input == nil {
			missing := "n"
			if op.n != nil {
				missing = "input"
			}
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("Missing value for '%s'", missing),
				name,
			)
		}
		return op, nil
	}
}

// Process returns n elements of the input array: the greatest or least
// non-null ones in order for $maxN and $minN, the first or last ones for
// $firstN and $lastN.
func (op *nOp) Process(doc *types.Document) (any, error) {
	nv, err := evalArgValue(op.n, doc)
	if err != nil {
		return nil, err
	}
	n, err := integralN(nv, op.name)
	if err != nil {
		return nil, err
	}
	iv, err := evalArgValue(op.input, doc)
	if err != nil {
		return nil, err
	}
	arr, ok := iv.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNOperatorInputNotArray, "Input must be an array", op.name,
		)
	}
	values := make([]any, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		values = append(values, must.NotFail(arr.Get(i)))
	}
	switch op.name {
	case "$maxN", "$minN":
		values = slices.DeleteFunc(values, isNullish)
		order := types.Ascending
		if op.name == "$maxN" {
			order = types.Descending
		}
		slices.SortStableFunc(values, func(a, b any) int {
			c := types.CompareOrder(a, b, types.Ascending)
			if order == types.Descending {
				c = -c
			}
			return int(c)
		})
		values = values[:min(int64(len(values)), n)]
	case "$firstN":
		values = values[:min(int64(len(values)), n)]
	case "$lastN":
		values = values[int64(len(values))-min(int64(len(values)), n):]
	}
	out := types.MakeArray(len(values))
	for _, v := range values {
		out.Append(v)
	}
	return out, nil
}

// integralN reads the 'n' of an N operator, which must be a positive integral
// number.
func integralN(v any, name string) (int64, error) {
	var n int64
	switch x := v.(type) {
	case int32:
		n = int64(x)
	case int64:
		n = x
	case float64:
		if x != math.Trunc(x) {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNOperatorNFractional,
				fmt.Sprintf("Value for 'n' must be of integral type, but found %v", x),
				name,
			)
		}
		n = int64(x)
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNOperatorNNotIntegral,
			fmt.Sprintf("Value for 'n' must be of integral type, but found %s", types.FormatAnyValue(v)),
			name,
		)
	}
	if n < 1 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNOperatorNNotPositive,
			fmt.Sprintf("'n' must be greater than 0, found %d", n),
			name,
		)
	}
	return n, nil
}

// percentileOp represents { $percentile: { input, p, method } } and
// { $median: { input, method } }.
type percentileOp struct {
	name   string
	input  any
	p      any
	median bool
}

func newPercentileFunc(name string, median bool) newOperatorFunc {
	return func(args ...any) (Operator, error) {
		spec, ok := singleDocumentArg(args)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("specification must be an object; found %v", argsForMessage(args)),
				name,
			)
		}
		op := &percentileOp{name: name, median: median}
		var method any
		for _, key := range spec.Keys() {
			v := must.NotFail(spec.Get(key))
			switch {
			case key == "input":
				op.input = v
			case key == "p" && !median:
				op.p = v
			case key == "method":
				method = v
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFailedToParse,
					fmt.Sprintf("BSON field '%s.%s' is an unknown field.", name, key),
					name,
				)
			}
		}
		if method != "approximate" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"Currently only 'approximate' can be used as percentile 'method'.",
				name,
			)
		}
		if op.input == nil || (!median && op.p == nil) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("BSON field '%s' is missing a required field", name),
				name,
			)
		}
		return op, nil
	}
}

// Process returns the percentiles of the numeric input values, as doubles, or
// null when there are none. A percentile p is the value at rank ceil(p*n).
func (op *percentileOp) Process(doc *types.Document) (any, error) {
	values, err := accumulatorValues([]any{op.input}, doc)
	if err != nil {
		return nil, err
	}
	nums := numericValues(values)
	slices.Sort(nums)
	at := func(p float64) any {
		if len(nums) == 0 {
			return types.Null
		}
		i := int(math.Ceil(p*float64(len(nums)))) - 1
		return nums[max(0, min(i, len(nums)-1))]
	}
	if op.median {
		return at(0.5), nil
	}
	pv, err := evalArgValue(op.p, doc)
	if err != nil {
		return nil, err
	}
	ps, ok := pv.(*types.Array)
	if !ok || ps.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("The 'p' field must be an array of numbers, but found %s", types.FormatAnyValue(pv)),
			op.name,
		)
	}
	out := types.MakeArray(ps.Len())
	for i := 0; i < ps.Len(); i++ {
		p, ok := numericValue(must.NotFail(ps.Get(i)))
		if !ok || p < 0 || p > 1 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"The 'p' field must be an array of numbers in [0.0, 1.0]",
				op.name,
			)
		}
		out.Append(at(p))
	}
	return out, nil
}

func numericValue(v any) (float64, bool) {
	nums := numericValues([]any{v})
	if len(nums) == 0 {
		return 0, false
	}
	return nums[0], true
}

func singleDocumentArg(args []any) (*types.Document, bool) {
	if len(args) != 1 {
		return nil, false
	}
	doc, ok := args[0].(*types.Document)
	return doc, ok
}

func argsForMessage(args []any) string {
	if len(args) == 1 {
		return types.FormatAnyValue(args[0])
	}
	arr := types.MakeArray(len(args))
	for _, a := range args {
		arr.Append(a)
	}
	return types.FormatAnyValue(arr)
}
