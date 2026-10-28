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
	"strconv"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// trigBounds is the domain of a trigonometric operator, checked as MongoDB's
// ExpressionBoundedTrigonometric does after NaN passes through.
type trigBounds struct {
	lower, upper float64
	inclusive    bool
}

var unboundedTrig = trigBounds{lower: math.Inf(-1), upper: math.Inf(1)}

func (b trigBounds) containsDouble(x float64) bool {
	if b.inclusive {
		return x >= b.lower && x <= b.upper
	}
	return x > b.lower && x < b.upper
}

func (b trigBounds) containsDecimal(x *apd.Decimal) bool {
	lower := x.Cmp(boundToAPD(b.lower))
	upper := x.Cmp(boundToAPD(b.upper))
	if b.inclusive {
		return lower >= 0 && upper <= 0
	}
	return lower > 0 && upper < 0
}

func boundToAPD(f float64) *apd.Decimal {
	if math.IsInf(f, 0) {
		return &apd.Decimal{Form: apd.Infinite, Negative: f < 0}
	}
	return aggregations.DoubleToAPD(f)
}

func (b trigBounds) String() string {
	open, closing := "(", ")"
	if b.inclusive {
		open, closing = "[", "]"
	}
	return open + formatBound(b.lower) + "," + formatBound(b.upper) + closing
}

func formatBound(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	default:
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

type trigOp struct {
	name    string
	arg     any
	bounds  trigBounds
	double  func(float64) float64
	decimal func(types.Decimal128) types.Decimal128
}

func newTrigOperator(
	name string, bounds trigBounds, double func(float64) float64, decimal func(types.Decimal128) types.Decimal128,
) newOperatorFunc {
	return func(args ...any) (Operator, error) {
		if len(args) != 1 {
			return nil, newOperatorError(ErrArgsInvalidLen, name,
				fmt.Sprintf("Expression %s takes exactly 1 argument. %d were passed in.", name, len(args)))
		}

		return &trigOp{name: name, arg: args[0], bounds: bounds, double: double, decimal: decimal}, nil
	}
}

func (op *trigOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}
	if !isNumber(v) {
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, op.name, v)
	}

	if d, ok := v.(types.Decimal128); ok {
		x := aggregations.DecimalToAPD(d)
		if x.Form == apd.NaN {
			return d, nil
		}
		if !op.bounds.containsDecimal(x) {
			return nil, op.outOfBounds(x.String())
		}
		return op.decimal(d), nil
	}

	f := toFloat64(v)
	if math.IsNaN(f) {
		return v, nil
	}
	if !op.bounds.containsDouble(f) {
		return nil, op.outOfBounds(formatBound(f))
	}
	return op.double(f), nil
}

func (op *trigOp) outOfBounds(value string) error {
	return mathError(handlererrors.ErrTrigOutOfBounds, op.name,
		fmt.Sprintf("cannot apply %s to %s, value must be in %s", op.name, value, op.bounds))
}

var _ Operator = (*trigOp)(nil)

func trigDouble(fn aggregations.TrigFunc) func(float64) float64 {
	return func(x float64) float64 { return aggregations.TrigDouble(fn, x) }
}

func trigDecimal(fn aggregations.TrigFunc) func(types.Decimal128) types.Decimal128 {
	return func(d types.Decimal128) types.Decimal128 { return aggregations.DecimalTrig(fn, d) }
}

var (
	newSin = newTrigOperator("$sin", unboundedTrig, trigDouble(aggregations.Sin), trigDecimal(aggregations.Sin))
	newCos = newTrigOperator("$cos", unboundedTrig, trigDouble(aggregations.Cos), trigDecimal(aggregations.Cos))
	newTan = newTrigOperator("$tan", unboundedTrig, trigDouble(aggregations.Tan), trigDecimal(aggregations.Tan))
)
