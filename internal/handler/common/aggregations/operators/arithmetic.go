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
	"time"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// addOp represents the $add expression operator.
//
//	{ $add: [ <expression1>, <expression2>, ... ] }
type addOp struct {
	args []any
}

func newAdd(args ...any) (Operator, error) {
	return &addOp{args: args}, nil
}

// Process implements Operator. Returns the sum of all evaluated args.
// Returns null if any arg evaluates to null.
// Non-numeric values are ignored (treated as zero by SumNumbers).
func (a *addOp) Process(doc *types.Document) (any, error) {
	var values []any

	for _, arg := range a.args {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}

		if isNullish(v) {
			return types.Null, nil
		}

		values = append(values, v)
	}

	return aggregations.SumNumbers(values...), nil
}

var _ Operator = (*addOp)(nil)

// subtractOp represents the $subtract expression operator.
//
//	{ $subtract: [ <expression1>, <expression2> ] }
type subtractOp struct {
	minuend    any
	subtrahend any
}

func newSubtract(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$subtract",
			fmt.Sprintf("Expression $subtract takes exactly 2 arguments. %d were passed in.", len(args)),
		)
	}

	return &subtractOp{minuend: args[0], subtrahend: args[1]}, nil
}

// Process implements Operator. Returns minuend - subtrahend.
// Returns null if either arg is null.
func (s *subtractOp) Process(doc *types.Document) (any, error) {
	a, err := evalArgValue(s.minuend, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(s.subtrahend, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(a) || isNullish(b) {
		return types.Null, nil
	}

	return subtractValues(a, b)
}

// subtractValues computes a - b as MongoDB does: numbers promote int to long
// to double on overflow and to Decimal128 when either is one; a date minus a
// date is their difference in milliseconds, and a date minus a number is a
// date. Anything else is a TypeMismatch.
func subtractValues(a, b any) (any, error) {
	if date, ok := a.(time.Time); ok {
		return subtractFromDate(date, b)
	}

	if !isNumber(a) || !isNumber(b) {
		return nil, mathError(handlererrors.ErrTypeMismatch, "$subtract", fmt.Sprintf(
			"can't $subtract %s from %s", handlerparams.AliasFromType(b), handlerparams.AliasFromType(a),
		))
	}

	_, aDec := a.(types.Decimal128)
	_, bDec := b.(types.Decimal128)
	if aDec || bDec {
		x, _ := aggregations.NumberToAPD(a)
		y, _ := aggregations.NumberToAPD(b)
		r := new(apd.Decimal)
		_, _ = aggregations.DecimalWorkContext().Sub(r, x, y)
		return aggregations.APDToDecimal128(r), nil
	}

	_, aFloat := a.(float64)
	_, bFloat := b.(float64)
	if aFloat || bFloat {
		return toFloat64(a) - toFloat64(b), nil
	}

	x, y := toInt64(a), toInt64(b)
	r := x - y
	if (x >= 0) != (y >= 0) && (r >= 0) != (x >= 0) {
		return float64(x) - float64(y), nil
	}
	_, a32 := a.(int32)
	_, b32 := b.(int32)
	if a32 && b32 && r >= math.MinInt32 && r <= math.MaxInt32 {
		return int32(r), nil
	}
	return r, nil
}

func subtractFromDate(date time.Time, b any) (any, error) {
	ms := date.UnixMilli()
	overflow := mathError(handlererrors.ErrOverflow, "$subtract", "date overflow")

	switch n := b.(type) {
	case time.Time:
		return ms - n.UnixMilli(), nil
	case int32, int64:
		y := toInt64(n)
		r := ms - y
		if (ms >= 0) != (y >= 0) && (r >= 0) != (ms >= 0) {
			return nil, overflow
		}
		return time.UnixMilli(r).UTC(), nil
	case float64:
		r := math.Round(float64(ms) - n)
		if math.IsNaN(r) || r >= math.MaxInt64 || r < math.MinInt64 {
			return nil, overflow
		}
		return time.UnixMilli(int64(r)).UTC(), nil
	case types.Decimal128:
		ctx := aggregations.DecimalWorkContext()
		diff, rounded := new(apd.Decimal), new(apd.Decimal)
		_, _ = ctx.Sub(diff, apd.New(ms, 0), aggregations.DecimalToAPD(n))
		_, _ = ctx.RoundToIntegralValue(rounded, diff)
		r, err := rounded.Int64()
		if err != nil {
			return nil, overflow
		}
		return time.UnixMilli(r).UTC(), nil
	default:
		return nil, mathError(handlererrors.ErrTypeMismatch, "$subtract", fmt.Sprintf(
			"can't $subtract %s from Date", handlerparams.AliasFromType(b),
		))
	}
}

// divideOp represents the $divide expression operator.
//
//	{ $divide: [ <expression1>, <expression2> ] }
type divideOp struct {
	dividend any
	divisor  any
}

func newDivide(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$divide",
			fmt.Sprintf("Expression $divide takes exactly 2 arguments. %d were passed in.", len(args)),
		)
	}

	return &divideOp{dividend: args[0], divisor: args[1]}, nil
}

// Process implements Operator. As in MongoDB, the quotient is a double unless
// either operand is a Decimal128, dividing by any zero is BadValue, and
// non-numeric operands are a TypeMismatch. Returns null if either arg is null.
func (d *divideOp) Process(doc *types.Document) (any, error) {
	a, err := evalArgValue(d.dividend, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(d.divisor, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(a) || isNullish(b) {
		return types.Null, nil
	}
	if !isNumber(a) || !isNumber(b) {
		return nil, mathError(handlererrors.ErrTypeMismatch, "$divide", fmt.Sprintf(
			"$divide only supports numeric types, not %s and %s", handlerparams.AliasFromType(a), handlerparams.AliasFromType(b),
		))
	}

	divideByZero := mathError(handlererrors.ErrBadValue, "$divide", "can't $divide by zero")

	_, aDec := a.(types.Decimal128)
	_, bDec := b.(types.Decimal128)
	if aDec || bDec {
		x, _ := aggregations.NumberToAPD(a)
		y, _ := aggregations.NumberToAPD(b)
		if y.Form == apd.Finite && y.IsZero() {
			return nil, divideByZero
		}
		r := new(apd.Decimal)
		_, _ = aggregations.DecimalWorkContext().Quo(r, x, y)
		return aggregations.DecimalResult(r, x.Exponent-y.Exponent), nil
	}

	divisor := toFloat64(b)
	if divisor == 0 {
		return nil, divideByZero
	}

	return toFloat64(a) / divisor, nil
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}

	return 0
}

// toBoundedInt converts a numeric index or length argument, saturating at the
// int32 range MongoDB accepts. Unlike int(toFloat64(v)) it is defined for NaN
// and huge values, and sums of two results cannot overflow int.
func toBoundedInt(v any) int {
	f := toFloat64(v)
	switch {
	case math.IsNaN(f):
		return 0
	case f > math.MaxInt32:
		return math.MaxInt32
	case f < math.MinInt32:
		return math.MinInt32
	}
	return int(f)
}

var (
	_ Operator = (*subtractOp)(nil)
	_ Operator = (*divideOp)(nil)
)
