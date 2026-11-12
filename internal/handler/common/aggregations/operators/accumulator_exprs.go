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
	"strconv"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
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
		case types.Decimal128:
			if f, err := aggregations.DecimalToAPD(n).Float64(); err == nil {
				nums = append(nums, f)
			}
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
	case "$minN":
		// The n smallest, of tied values the earliest, as mongod keeps them.
		values = slices.DeleteFunc(values, isNullish)
		slices.SortStableFunc(values, func(a, b any) int { return int(types.CompareOrder(a, b, types.Ascending)) })
		values = values[:min(int64(len(values)), n)]
	case "$maxN":
		values = largestN(slices.DeleteFunc(values, isNullish), n)
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

// IntegralN validates the 'n' of an N operator or accumulator as mongod's
// classic engine does.
func IntegralN(v any, name string) (int64, error) {
	return integralN(v, name)
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
		if math.IsNaN(x) || math.IsInf(x, 0) || x >= math.MaxInt64 || x < math.MinInt64 {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrCoerceLongOutOfRange,
				fmt.Sprintf("Can't coerce out of range value %s to long", formatCoercedDouble(x)),
				name,
			)
		}
		if x != math.Trunc(x) {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNOperatorNFractional,
				fmt.Sprintf("Value for 'n' must be of integral type, but found %v", x),
				name,
			)
		}
		n = int64(x)
	case types.Decimal128:
		i, ok := aggregations.Decimal128ToInt64(x)
		if !ok {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNOperatorNFractional,
				fmt.Sprintf("Value for 'n' must be of integral type, but found %s", types.FormatAnyValue(x)),
				name,
			)
		}
		n = i
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

// formatCoercedDouble formats a double as mongod's out of range error does.
func formatCoercedDouble(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// largestN returns the n largest values, greatest first, as mongod's $maxN
// keeps them: a value that only ties the least kept one is refused, and a
// greater one evicts the earliest of the least.
func largestN(values []any, n int64) []any {
	type entry struct {
		v   any
		seq int
	}
	order := func(a, b entry) int {
		if c := types.CompareOrder(a.v, b.v, types.Ascending); c != types.Equal {
			return int(c)
		}
		return a.seq - b.seq
	}
	kept := make([]entry, 0, min(int64(len(values)), n))
	// kept is a heap whose root is the least, earliest entry.
	down := func(i int) {
		for {
			least, l, r := i, 2*i+1, 2*i+2
			if l < len(kept) && order(kept[l], kept[least]) < 0 {
				least = l
			}
			if r < len(kept) && order(kept[r], kept[least]) < 0 {
				least = r
			}
			if least == i {
				return
			}
			kept[i], kept[least] = kept[least], kept[i]
			i = least
		}
	}
	for i, v := range values {
		e := entry{v: v, seq: i}
		if int64(len(kept)) < n {
			kept = append(kept, e)
			for j := len(kept) - 1; j > 0 && order(kept[j], kept[(j-1)/2]) < 0; j = (j - 1) / 2 {
				kept[j], kept[(j-1)/2] = kept[(j-1)/2], kept[j]
			}
			continue
		}
		if types.CompareOrder(v, kept[0].v, types.Ascending) == types.Greater {
			kept[0] = e
			down(0)
		}
	}
	slices.SortFunc(kept, func(a, b entry) int { return -order(a, b) })
	out := make([]any, len(kept))
	for i, e := range kept {
		out[i] = e.v
	}
	return out
}

// percentileOp represents { $percentile: { input, p, method } } and
// { $median: { input, method } }.
type percentileOp struct {
	spec   *PercentileSpec
	median bool
}

func newPercentileFunc(name string, median bool) newOperatorFunc {
	notObject := handlererrors.ErrPercentileExprNotObject
	if median {
		notObject = handlererrors.ErrMedianExprNotObject
	}
	return func(args ...any) (Operator, error) {
		var arg any = types.MakeArray(0)
		if len(args) == 1 {
			arg = args[0]
		} else if len(args) > 1 {
			arr := types.MakeArray(len(args))
			for _, a := range args {
				arr.Append(a)
			}
			arg = arr
		}
		spec, err := ParsePercentileSpec(name, median, arg, notObject)
		if err != nil {
			return nil, err
		}
		return &percentileOp{spec: spec, median: median}, nil
	}
}

// Process returns the percentiles of the numeric input values, as doubles, or
// null when there are none.
func (op *percentileOp) Process(doc *types.Document) (any, error) {
	values, err := accumulatorValues([]any{op.spec.Input}, doc)
	if err != nil {
		return nil, err
	}
	nums := numericValues(values)
	slices.Sort(nums)
	if op.median {
		return Percentile(nums, 0.5), nil
	}
	out := types.MakeArray(len(op.spec.Ps))
	for _, p := range op.spec.Ps {
		out.Append(Percentile(nums, p))
	}
	return out, nil
}

// NumericValue returns a number as a double.
func NumericValue(v any) (float64, bool) {
	return numericValue(v)
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
