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
	"math/big"
	"strconv"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// The math operators follow MongoDB: non-numeric operands and arguments
// outside an operator's domain are errors with MongoDB's codes, integer
// results that overflow are promoted (int to long to double), and Decimal128
// operands are computed in Decimal128. NaN passes through.

func mathError(code handlererrors.ErrorCode, op, msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, msg, op)
}

func isNumber(v any) bool {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return true
	default:
		return false
	}
}

func nonNumericError(code handlererrors.ErrorCode, op string, v any) error {
	return mathError(code, op, fmt.Sprintf("%s only supports numeric types, not %s", op, handlerparams.AliasFromType(v)))
}

func formatMathValue(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'g', -1, 64)
	case types.Decimal128:
		return aggregations.DecimalToAPD(n).String()
	default:
		return fmt.Sprint(v)
	}
}

// isNonPositive reports whether a numeric value is zero or negative; NaN is not.
func isNonPositive(v any) bool {
	if d, ok := v.(types.Decimal128); ok {
		x := aggregations.DecimalToAPD(d)
		return x.Form != apd.NaN && (x.Negative || x.IsZero())
	}
	return toFloat64(v) <= 0
}

type absOp struct{ arg any }

func newAbs(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$abs",
			fmt.Sprintf("Expression $abs takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &absOp{arg: args[0]}, nil
}

func (op *absOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	switch n := v.(type) {
	case types.NullType, types.UndefinedType:
		return types.Null, nil
	case float64:
		return math.Abs(n), nil
	case int32:
		if n == math.MinInt32 {
			return -int64(n), nil
		}
		if n < 0 {
			return -n, nil
		}
		return n, nil
	case int64:
		if n == math.MinInt64 {
			return nil, mathError(handlererrors.ErrAbsOfLongMin, "$abs", "can't take $abs of long long min")
		}
		if n < 0 {
			return -n, nil
		}
		return n, nil
	case types.Decimal128:
		return aggregations.APDToDecimal128(new(apd.Decimal).Abs(aggregations.DecimalToAPD(n))), nil
	default:
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, "$abs", v)
	}
}

var _ Operator = (*absOp)(nil)

type ceilOp struct{ arg any }

func newCeil(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$ceil",
			fmt.Sprintf("Expression $ceil takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &ceilOp{arg: args[0]}, nil
}

func (op *ceilOp) Process(doc *types.Document) (any, error) {
	return roundToIntegral(op.arg, doc, "$ceil", math.Ceil, (*apd.Context).Ceil)
}

var _ Operator = (*ceilOp)(nil)

type floorOp struct{ arg any }

func newFloor(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$floor",
			fmt.Sprintf("Expression $floor takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &floorOp{arg: args[0]}, nil
}

func (op *floorOp) Process(doc *types.Document) (any, error) {
	return roundToIntegral(op.arg, doc, "$floor", math.Floor, (*apd.Context).Floor)
}

var _ Operator = (*floorOp)(nil)

func roundToIntegral(
	arg any, doc *types.Document, name string,
	double func(float64) float64,
	decimal func(*apd.Context, *apd.Decimal, *apd.Decimal) (apd.Condition, error),
) (any, error) {
	v, err := evalArgValue(arg, doc)
	if err != nil {
		return nil, err
	}

	switch n := v.(type) {
	case types.NullType, types.UndefinedType:
		return types.Null, nil
	case float64:
		return double(n), nil
	case int32, int64:
		return n, nil
	case types.Decimal128:
		x := aggregations.DecimalToAPD(n)
		if x.Form != apd.Finite {
			return n, nil
		}
		r := new(apd.Decimal)
		if _, err = decimal(aggregations.DecimalWorkContext(), r, x); err != nil {
			return n, nil
		}
		return aggregations.APDToDecimal128(r), nil
	default:
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, name, v)
	}
}

type sqrtOp struct{ arg any }

func newSqrt(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$sqrt",
			fmt.Sprintf("Expression $sqrt takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &sqrtOp{arg: args[0]}, nil
}

func (op *sqrtOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}
	if !isNumber(v) {
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, "$sqrt", v)
	}
	if d, ok := v.(types.Decimal128); ok {
		x := aggregations.DecimalToAPD(d)
		if x.Form != apd.NaN && x.Negative && !x.IsZero() {
			return nil, mathError(handlererrors.ErrSqrtNegative, "$sqrt", "$sqrt's argument must be greater than or equal to 0")
		}
		r := new(apd.Decimal)
		if _, err = aggregations.DecimalWorkContext().Sqrt(r, x); err != nil {
			return d, nil
		}
		// The ideal exponent of a square root is half the argument's, rounded down.
		ideal := x.Exponent / 2
		if x.Exponent < 0 && x.Exponent%2 != 0 {
			ideal--
		}
		return aggregations.DecimalResult(r, ideal), nil
	}

	f := toFloat64(v)
	if f < 0 {
		return nil, mathError(handlererrors.ErrSqrtNegative, "$sqrt", "$sqrt's argument must be greater than or equal to 0")
	}
	return math.Sqrt(f), nil
}

var _ Operator = (*sqrtOp)(nil)

type expOp struct{ arg any }

func newExp(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$exp",
			fmt.Sprintf("Expression $exp takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &expOp{arg: args[0]}, nil
}

func (op *expOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}
	if !isNumber(v) {
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, "$exp", v)
	}
	if d, ok := v.(types.Decimal128); ok {
		r := new(apd.Decimal)
		if _, err = aggregations.DecimalWorkContext().Exp(r, aggregations.DecimalToAPD(d)); err != nil {
			return d, nil
		}
		return aggregations.DecimalResult(r, 0), nil
	}

	return math.Exp(toFloat64(v)), nil
}

var _ Operator = (*expOp)(nil)

// logarithm computes $ln and $log10, which need a positive argument.
func logarithm(
	arg any, doc *types.Document, name string, code handlererrors.ErrorCode,
	double func(float64) float64,
	decimal func(*apd.Context, *apd.Decimal, *apd.Decimal) (apd.Condition, error),
) (any, error) {
	v, err := evalArgValue(arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}
	if !isNumber(v) {
		return nil, nonNumericError(handlererrors.ErrMathNonNumeric, name, v)
	}
	if isNonPositive(v) {
		return nil, mathError(code, name, fmt.Sprintf("%s's argument must be a positive number, but is %s", name, formatMathValue(v)))
	}
	if d, ok := v.(types.Decimal128); ok {
		x := aggregations.DecimalToAPD(d)
		if x.Form != apd.Finite {
			return d, nil
		}
		r := new(apd.Decimal)
		if _, err = decimal(aggregations.DecimalWorkContext(), r, x); err != nil {
			return d, nil
		}
		return aggregations.DecimalResult(r, 0), nil
	}

	return double(toFloat64(v)), nil
}

type lnOp struct{ arg any }

func newLn(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$ln",
			fmt.Sprintf("Expression $ln takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &lnOp{arg: args[0]}, nil
}

func (op *lnOp) Process(doc *types.Document) (any, error) {
	return logarithm(op.arg, doc, "$ln", handlererrors.ErrLnNonPositive, math.Log, (*apd.Context).Ln)
}

var _ Operator = (*lnOp)(nil)

type log10Op struct{ arg any }

func newLog10(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$log10",
			fmt.Sprintf("Expression $log10 takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &log10Op{arg: args[0]}, nil
}

func (op *log10Op) Process(doc *types.Document) (any, error) {
	return logarithm(op.arg, doc, "$log10", handlererrors.ErrLog10NonPositive, math.Log10, (*apd.Context).Log10)
}

var _ Operator = (*log10Op)(nil)

// logOp represents { $log: [ <number>, <base> ] }.
type logOp struct {
	number any
	base   any
}

func newLog(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$log",
			fmt.Sprintf("Expression $log takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &logOp{number: args[0], base: args[1]}, nil
}

func (op *logOp) Process(doc *types.Document) (any, error) {
	n, err := evalArgValue(op.number, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(op.base, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(n) || isNullish(b) {
		return types.Null, nil
	}
	if !isNumber(n) {
		return nil, mathError(handlererrors.ErrLogNonNumericArgument, "$log",
			fmt.Sprintf("$log's argument must be numeric, not %s", handlerparams.AliasFromType(n)))
	}
	if !isNumber(b) {
		return nil, mathError(handlererrors.ErrLogNonNumericBase, "$log",
			fmt.Sprintf("$log's base must be numeric, not %s", handlerparams.AliasFromType(b)))
	}
	if isNonPositive(n) {
		return nil, mathError(handlererrors.ErrLogNonPositiveArgument, "$log",
			fmt.Sprintf("$log's argument must be a positive number, but is %s", formatMathValue(n)))
	}
	if isNonPositive(b) || isOne(b) {
		return nil, mathError(handlererrors.ErrLogInvalidBase, "$log",
			fmt.Sprintf("$log's base must be a positive number not equal to 1, but is %s", formatMathValue(b)))
	}

	_, nDec := n.(types.Decimal128)
	_, bDec := b.(types.Decimal128)
	if nDec || bDec {
		x, _ := aggregations.NumberToAPD(n)
		base, _ := aggregations.NumberToAPD(b)
		if x.Form != apd.Finite || base.Form != apd.Finite {
			return aggregations.APDToDecimal128(&apd.Decimal{Form: apd.NaN}), nil
		}
		ctx := aggregations.DecimalWorkContext()
		lnX, lnBase, r := new(apd.Decimal), new(apd.Decimal), new(apd.Decimal)
		_, _ = ctx.Ln(lnX, x)
		_, _ = ctx.Ln(lnBase, base)
		_, _ = ctx.Quo(r, lnX, lnBase)
		return aggregations.DecimalResult(r, 0), nil
	}

	return math.Log(toFloat64(n)) / math.Log(toFloat64(b)), nil
}

func isOne(v any) bool {
	if d, ok := v.(types.Decimal128); ok {
		return aggregations.DecimalToAPD(d).Cmp(apd.New(1, 0)) == 0
	}
	return toFloat64(v) == 1
}

var _ Operator = (*logOp)(nil)

// powOp represents { $pow: [ <base>, <exponent> ] }.
type powOp struct {
	base     any
	exponent any
}

func newPow(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$pow",
			fmt.Sprintf("Expression $pow takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &powOp{base: args[0], exponent: args[1]}, nil
}

func (op *powOp) Process(doc *types.Document) (any, error) {
	b, err := evalArgValue(op.base, doc)
	if err != nil {
		return nil, err
	}

	e, err := evalArgValue(op.exponent, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(b) || isNullish(e) {
		return types.Null, nil
	}
	if !isNumber(b) {
		return nil, mathError(handlererrors.ErrPowNonNumericBase, "$pow",
			fmt.Sprintf("$pow's base must be numeric, not %s", handlerparams.AliasFromType(b)))
	}
	if !isNumber(e) {
		return nil, mathError(handlererrors.ErrPowNonNumericExponent, "$pow",
			fmt.Sprintf("$pow's exponent must be numeric, not %s", handlerparams.AliasFromType(e)))
	}

	zeroBaseError := mathError(handlererrors.ErrPowZeroNegativeExponent, "$pow", "$pow cannot take a base of 0 and a negative exponent")

	_, bDec := b.(types.Decimal128)
	_, eDec := e.(types.Decimal128)
	if bDec || eDec {
		base, _ := aggregations.NumberToAPD(b)
		exponent, _ := aggregations.NumberToAPD(e)
		if base.Form == apd.Finite && base.IsZero() && exponent.Negative && !exponent.IsZero() {
			return nil, zeroBaseError
		}
		r := new(apd.Decimal)
		if _, err = aggregations.DecimalWorkContext().Pow(r, base, exponent); err != nil {
			r = &apd.Decimal{Form: apd.NaN}
		}
		return aggregations.DecimalFullPrecisionResult(r), nil
	}

	if isIntegral(b) && isIntegral(e) {
		return integerPow(b, e, zeroBaseError)
	}

	bf, ef := toFloat64(b), toFloat64(e)
	if bf == 0 && ef < 0 {
		return nil, zeroBaseError
	}
	return math.Pow(bf, ef), nil
}

func isIntegral(v any) bool {
	switch v.(type) {
	case int32, int64:
		return true
	default:
		return false
	}
}

// integerPow raises an int or long to an int or long power as MongoDB does:
// an int when both operands are ints and the result fits, otherwise a long
// when it fits, otherwise a double. Negative exponents give a double except
// for bases 1 and -1.
func integerPow(b, e any, zeroBaseError error) (any, error) {
	_, b32 := b.(int32)
	_, e32 := e.(int32)
	bothInt := b32 && e32
	base, exponent := toInt64(b), toInt64(e)

	typed := func(n int64) any {
		if bothInt && n >= math.MinInt32 && n <= math.MaxInt32 {
			return int32(n)
		}
		return n
	}

	if exponent < 0 {
		switch base {
		case 0:
			return nil, zeroBaseError
		case 1:
			return typed(1), nil
		case -1:
			if exponent%2 == 0 {
				return typed(1), nil
			}
			return typed(-1), nil
		default:
			return math.Pow(float64(base), float64(exponent)), nil
		}
	}

	if base > 1 || base < -1 {
		if exponent >= 64 {
			return math.Pow(float64(base), float64(exponent)), nil
		}
	}
	r := new(big.Int).Exp(big.NewInt(base), big.NewInt(exponent), nil)
	if !r.IsInt64() {
		return math.Pow(float64(base), float64(exponent)), nil
	}
	return typed(r.Int64()), nil
}

var _ Operator = (*powOp)(nil)

// truncOp represents { $trunc: <number> } or { $trunc: [ <number>, <place> ] }.
type truncOp struct {
	number any
	place  any // nil means 0 decimal places
}

func newTrunc(args ...any) (Operator, error) {
	switch len(args) {
	case 1:
		return &truncOp{number: args[0]}, nil
	case 2:
		return &truncOp{number: args[0], place: args[1]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$trunc",
			fmt.Sprintf("Expression $trunc takes 1 or 2 arguments. %d were passed in.", len(args)))
	}
}

func (op *truncOp) Process(doc *types.Document) (any, error) {
	return roundToPlace(op.number, op.place, doc, "$trunc", false)
}

var _ Operator = (*truncOp)(nil)

// roundOp represents { $round: <number> } or { $round: [ <number>, <place> ] }.
type roundOp struct {
	number any
	place  any
}

func newRound(args ...any) (Operator, error) {
	switch len(args) {
	case 1:
		return &roundOp{number: args[0]}, nil
	case 2:
		return &roundOp{number: args[0], place: args[1]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$round",
			fmt.Sprintf("Expression $round takes 1 or 2 arguments. %d were passed in.", len(args)))
	}
}

func (op *roundOp) Process(doc *types.Document) (any, error) {
	return roundToPlace(op.number, op.place, doc, "$round", true)
}

var _ Operator = (*roundOp)(nil)

// roundToPlace implements $round (half to even) and $trunc (toward zero) to
// place decimal digits, which MongoDB requires to be an integer in [-20, 100].
func roundToPlace(numberArg, placeArg any, doc *types.Document, name string, halfEven bool) (any, error) {
	v, err := evalArgValue(numberArg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}

	var place int64
	if placeArg != nil {
		pv, err := evalArgValue(placeArg, doc)
		if err != nil {
			return nil, err
		}
		if isNullish(pv) {
			return types.Null, nil
		}

		var integral bool
		switch p := pv.(type) {
		case int32:
			place, integral = int64(p), true
		case int64:
			place, integral = p, true
		case float64:
			integral = p == math.Trunc(p) && !math.IsInf(p, 0) && math.Abs(p) < 1<<53
			place = int64(p)
		case types.Decimal128:
			place, integral = aggregations.Decimal128ToInt64(p)
		default:
			return nil, mathError(handlererrors.ErrCannotConvertToLong, name,
				fmt.Sprintf("can't convert from BSON type %s to long", handlerparams.AliasFromType(pv)))
		}
		if !integral {
			return nil, mathError(handlererrors.ErrRoundPlaceNotIntegral, name,
				fmt.Sprintf("precision argument to  %s must be a integral value", name))
		}
		if place < -20 || place > 100 {
			return nil, mathError(handlererrors.ErrRoundPlaceOutOfRange, name,
				fmt.Sprintf("cannot apply %s with precision value %d value must be in [-20, 100]", name, place))
		}
	}

	switch n := v.(type) {
	case int32, int64:
		return roundInteger(n, place, halfEven), nil

	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return n, nil
		}
		rnd := math.Trunc
		if halfEven {
			rnd = math.RoundToEven
		}
		if place == 0 {
			return rnd(n), nil
		}
		factor := math.Pow(10, float64(place))
		scaled := n * factor
		if math.IsInf(scaled, 0) {
			return n, nil
		}
		return rnd(scaled) / factor, nil

	case types.Decimal128:
		x := aggregations.DecimalToAPD(n)
		target := int32(-place)
		if x.Form != apd.Finite || x.Exponent >= target {
			return n, nil
		}
		ctx := aggregations.DecimalWorkContext()
		ctx.Rounding = apd.RoundDown
		if halfEven {
			ctx.Rounding = apd.RoundHalfEven
		}
		r := new(apd.Decimal)
		if _, err = ctx.Quantize(r, x, target); err != nil {
			return n, nil
		}
		return aggregations.APDToDecimal128(r), nil

	default:
		return nil, nonNumericError(handlererrors.ErrRoundNonNumeric, name, v)
	}
}

// roundInteger rounds an int or long to a negative place, keeping its type
// unless the result overflows it.
func roundInteger(v any, place int64, halfEven bool) any {
	if place >= 0 {
		return v
	}

	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(-place), nil)
	q, rem := new(big.Int).QuoRem(big.NewInt(toInt64(v)), unit, new(big.Int))
	if halfEven && rem.Sign() != 0 {
		twice := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2))
		if c := twice.Cmp(unit); c > 0 || (c == 0 && q.Bit(0) == 1) {
			q.Add(q, big.NewInt(int64(rem.Sign())))
		}
	}
	r := q.Mul(q, unit)

	if !r.IsInt64() {
		f, _ := new(big.Float).SetInt(r).Float64()
		return f
	}
	n := r.Int64()
	if _, ok := v.(int32); ok && n >= math.MinInt32 && n <= math.MaxInt32 {
		return int32(n)
	}
	return n
}

// modOp represents { $mod: [ <dividend>, <divisor> ] }.
type modOp struct {
	dividend any
	divisor  any
}

func newMod(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$mod",
			fmt.Sprintf("Expression $mod takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &modOp{dividend: args[0], divisor: args[1]}, nil
}

// Process implements Operator. As in MongoDB, the remainder takes the sign of
// the dividend and keeps integer types (int unless either operand is a long),
// is a double when either operand is, and a Decimal128 when either is one.
func (op *modOp) Process(doc *types.Document) (any, error) {
	a, err := evalArgValue(op.dividend, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(op.divisor, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(a) || isNullish(b) {
		return types.Null, nil
	}
	if !isNumber(a) || !isNumber(b) {
		return nil, mathError(handlererrors.ErrModNonNumeric, "$mod", fmt.Sprintf(
			"$mod only supports numeric types, not %s and %s", handlerparams.AliasFromType(a), handlerparams.AliasFromType(b),
		))
	}

	_, aDec := a.(types.Decimal128)
	_, bDec := b.(types.Decimal128)
	if aDec || bDec {
		x, _ := aggregations.NumberToAPD(a)
		y, _ := aggregations.NumberToAPD(b)
		if y.Form == apd.Finite && y.IsZero() {
			return nil, mathError(handlererrors.ErrModByZeroDecimal, "$mod", "can't $mod by zero")
		}
		r := new(apd.Decimal)
		if _, err = aggregations.DecimalWorkContext().Rem(r, x, y); err != nil {
			r = &apd.Decimal{Form: apd.NaN}
		}
		return aggregations.APDToDecimal128(r), nil
	}

	modByZero := mathError(handlererrors.ErrModByZero, "$mod", "can't $mod by zero")

	if isIntegral(a) && isIntegral(b) {
		divisor := toInt64(b)
		if divisor == 0 {
			return nil, modByZero
		}
		r := toInt64(a) % divisor
		_, a32 := a.(int32)
		_, b32 := b.(int32)
		if a32 && b32 {
			return int32(r), nil
		}
		return r, nil
	}

	divisor := toFloat64(b)
	if divisor == 0 {
		return nil, modByZero
	}

	return math.Mod(toFloat64(a), divisor), nil
}

var _ Operator = (*modOp)(nil)
