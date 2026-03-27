// Copyright 2021 FerretDB Inc.
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

	"github.com/dolthub/dongo/internal/types"
)

// ── $abs ─────────────────────────────────────────────────────────────────────

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

	if v == types.Null {
		return types.Null, nil
	}

	switch n := v.(type) {
	case float64:
		return math.Abs(n), nil
	case int32:
		if n < 0 {
			return -n, nil
		}

		return n, nil
	case int64:
		if n < 0 {
			return -n, nil
		}

		return n, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$abs",
			fmt.Sprintf("$abs only supports numeric types, not %T", v))
	}
}

var _ Operator = (*absOp)(nil)

// ── $ceil ─────────────────────────────────────────────────────────────────────

type ceilOp struct{ arg any }

func newCeil(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$ceil",
			fmt.Sprintf("Expression $ceil takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &ceilOp{arg: args[0]}, nil
}

func (op *ceilOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch n := v.(type) {
	case float64:
		return math.Ceil(n), nil
	case int32:
		return n, nil
	case int64:
		return n, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$ceil",
			fmt.Sprintf("$ceil only supports numeric types, not %T", v))
	}
}

var _ Operator = (*ceilOp)(nil)

// ── $floor ────────────────────────────────────────────────────────────────────

type floorOp struct{ arg any }

func newFloor(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$floor",
			fmt.Sprintf("Expression $floor takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &floorOp{arg: args[0]}, nil
}

func (op *floorOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch n := v.(type) {
	case float64:
		return math.Floor(n), nil
	case int32:
		return n, nil
	case int64:
		return n, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$floor",
			fmt.Sprintf("$floor only supports numeric types, not %T", v))
	}
}

var _ Operator = (*floorOp)(nil)

// ── $sqrt ─────────────────────────────────────────────────────────────────────

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

	if v == types.Null {
		return types.Null, nil
	}

	return math.Sqrt(toFloat64(v)), nil
}

var _ Operator = (*sqrtOp)(nil)

// ── $exp ──────────────────────────────────────────────────────────────────────

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

	if v == types.Null {
		return types.Null, nil
	}

	return math.Exp(toFloat64(v)), nil
}

var _ Operator = (*expOp)(nil)

// ── $log10 ────────────────────────────────────────────────────────────────────

type log10Op struct{ arg any }

func newLog10(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$log10",
			fmt.Sprintf("Expression $log10 takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &log10Op{arg: args[0]}, nil
}

func (op *log10Op) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	return math.Log10(toFloat64(v)), nil
}

var _ Operator = (*log10Op)(nil)

// ── $log ──────────────────────────────────────────────────────────────────────

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

	if n == types.Null || b == types.Null {
		return types.Null, nil
	}

	return math.Log(toFloat64(n)) / math.Log(toFloat64(b)), nil
}

var _ Operator = (*logOp)(nil)

// ── $pow ──────────────────────────────────────────────────────────────────────

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

	if b == types.Null || e == types.Null {
		return types.Null, nil
	}

	return math.Pow(toFloat64(b), toFloat64(e)), nil
}

var _ Operator = (*powOp)(nil)

// ── $trunc ────────────────────────────────────────────────────────────────────

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
	v, err := evalArgValue(op.number, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	place := 0
	if op.place != nil {
		pv, err := evalArgValue(op.place, doc)
		if err != nil {
			return nil, err
		}

		if pv != types.Null {
			place = int(toFloat64(pv))
		}
	}

	switch n := v.(type) {
	case int32, int64:
		return v, nil
	case float64:
		if place == 0 {
			return math.Trunc(n), nil
		}

		factor := math.Pow(10, float64(place))

		return math.Trunc(n*factor) / factor, nil
	default:
		return math.Trunc(toFloat64(v)), nil
	}
}

var _ Operator = (*truncOp)(nil)

// ── $round ────────────────────────────────────────────────────────────────────

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
	v, err := evalArgValue(op.number, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	place := 0
	if op.place != nil {
		pv, err := evalArgValue(op.place, doc)
		if err != nil {
			return nil, err
		}

		if pv != types.Null {
			place = int(toFloat64(pv))
		}
	}

	switch n := v.(type) {
	case int32, int64:
		return v, nil
	case float64:
		if place == 0 {
			return math.RoundToEven(n), nil
		}

		factor := math.Pow(10, float64(place))

		return math.RoundToEven(n*factor) / factor, nil
	default:
		return math.RoundToEven(toFloat64(v)), nil
	}
}

var _ Operator = (*roundOp)(nil)

// ── $mod ──────────────────────────────────────────────────────────────────────

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

func (op *modOp) Process(doc *types.Document) (any, error) {
	a, err := evalArgValue(op.dividend, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(op.divisor, doc)
	if err != nil {
		return nil, err
	}

	if a == types.Null || b == types.Null {
		return types.Null, nil
	}

	// Preserve integer types when possible.
	switch av := a.(type) {
	case int32:
		if bv, ok := b.(int32); ok {
			if bv == 0 {
				return nil, newOperatorError(ErrArgsInvalidLen, "$mod", "$mod by zero")
			}

			return av % bv, nil
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			if bv == 0 {
				return nil, newOperatorError(ErrArgsInvalidLen, "$mod", "$mod by zero")
			}

			return av % bv, nil
		case int32:
			if bv == 0 {
				return nil, newOperatorError(ErrArgsInvalidLen, "$mod", "$mod by zero")
			}

			return av % int64(bv), nil
		}
	}

	bf := toFloat64(b)
	if bf == 0 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$mod", "$mod by zero")
	}

	return math.Mod(toFloat64(a), bf), nil
}

var _ Operator = (*modOp)(nil)
