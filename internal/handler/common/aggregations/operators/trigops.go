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

var (
	finiteTrigBounds = &trigBounds{lower: math.Inf(-1), upper: math.Inf(1)}
	unitTrigBounds   = &trigBounds{lower: -1, upper: 1, inclusive: true}
	acoshTrigBounds  = &trigBounds{lower: 1, upper: math.Inf(1), inclusive: true}
)

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

// trigOp evaluates a single-argument trigonometric operator. Without bounds
// NaN and infinities reach the function, as in MongoDB's unbounded
// trigonometric expressions.
type trigOp struct {
	name    string
	arg     any
	bounds  *trigBounds
	double  func(float64) float64
	decimal func(types.Decimal128) types.Decimal128
}

func newTrigOperator(
	name string, bounds *trigBounds, double func(float64) float64, decimal func(types.Decimal128) types.Decimal128,
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
		if op.bounds == nil {
			return op.decimal(d), nil
		}
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
	if op.bounds == nil {
		return op.double(f), nil
	}
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
	newSin   = newTrigOperator("$sin", finiteTrigBounds, trigDouble(aggregations.Sin), trigDecimal(aggregations.Sin))
	newCos   = newTrigOperator("$cos", finiteTrigBounds, trigDouble(aggregations.Cos), trigDecimal(aggregations.Cos))
	newTan   = newTrigOperator("$tan", finiteTrigBounds, trigDouble(aggregations.Tan), trigDecimal(aggregations.Tan))
	newAsin  = newTrigOperator("$asin", unitTrigBounds, trigDouble(aggregations.Asin), trigDecimal(aggregations.Asin))
	newAcos  = newTrigOperator("$acos", unitTrigBounds, trigDouble(aggregations.Acos), trigDecimal(aggregations.Acos))
	newAtan  = newTrigOperator("$atan", nil, trigDouble(aggregations.Atan), trigDecimal(aggregations.Atan))
	newSinh  = newTrigOperator("$sinh", nil, trigDouble(aggregations.Sinh), trigDecimal(aggregations.Sinh))
	newCosh  = newTrigOperator("$cosh", nil, trigDouble(aggregations.Cosh), trigDecimal(aggregations.Cosh))
	newTanh  = newTrigOperator("$tanh", nil, trigDouble(aggregations.Tanh), trigDecimal(aggregations.Tanh))
	newAsinh = newTrigOperator("$asinh", nil, trigDouble(aggregations.Asinh), trigDecimal(aggregations.Asinh))
	newAcosh = newTrigOperator("$acosh", acoshTrigBounds, trigDouble(aggregations.Acosh), trigDecimal(aggregations.Acosh))
	newAtanh = newTrigOperator("$atanh", unitTrigBounds, trigDouble(aggregations.Atanh), trigDecimal(aggregations.Atanh))

	newDegreesToRadians = newTrigOperator("$degreesToRadians", nil,
		aggregations.DegreesToRadiansDouble, aggregations.DegreesToRadiansDecimal)
	newRadiansToDegrees = newTrigOperator("$radiansToDegrees", nil,
		aggregations.RadiansToDegreesDouble, aggregations.RadiansToDegreesDecimal)
)

// atan2Op represents { $atan2: [ <y>, <x> ] }. Like MongoDB's
// ExpressionTwoNumericArgs, y is checked before x is evaluated.
type atan2Op struct {
	y, x any
}

func newAtan2(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$atan2",
			fmt.Sprintf("Expression $atan2 takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &atan2Op{y: args[0], x: args[1]}, nil
}

func (op *atan2Op) Process(doc *types.Document) (any, error) {
	y, err := evalArgValue(op.y, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(y) {
		return types.Null, nil
	}
	if !isNumber(y) {
		return nil, nonNumericError(handlererrors.ErrNumericFirstArgument, "$atan2", y)
	}

	x, err := evalArgValue(op.x, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(x) {
		return types.Null, nil
	}
	if !isNumber(x) {
		return nil, nonNumericError(handlererrors.ErrNumericSecondArgument, "$atan2", x)
	}

	_, yDec := y.(types.Decimal128)
	_, xDec := x.(types.Decimal128)
	if yDec || xDec {
		return aggregations.DecimalAtan2(signedNumberToAPD(y), signedNumberToAPD(x)), nil
	}
	return aggregations.TrigAtan2Double(toFloat64(y), toFloat64(x)), nil
}

var _ Operator = (*atan2Op)(nil)

func signedNumberToAPD(v any) *apd.Decimal {
	if d, ok := v.(types.Decimal128); ok {
		return aggregations.SignedDecimalToAPD(d)
	}
	x, _ := aggregations.NumberToAPD(v)
	return x
}
