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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
)

// -- $add ---------------------------------------------------------------------

// addOp represents the $add expression operator.
//
//	{ $add: [ <expression1>, <expression2>, ... ] }
type addOp struct {
	args []any
}

// newAdd creates a new $add operator.
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

		if v == types.Null {
			return types.Null, nil
		}

		values = append(values, v)
	}

	return aggregations.SumNumbers(values...), nil
}

// check interfaces
var _ Operator = (*addOp)(nil)

// -- $subtract ----------------------------------------------------------------

// subtractOp represents the $subtract expression operator.
//
//	{ $subtract: [ <expression1>, <expression2> ] }
type subtractOp struct {
	minuend    any
	subtrahend any
}

// newSubtract creates a new $subtract operator.
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

	if a == types.Null || b == types.Null {
		return types.Null, nil
	}

	return subtractValues(a, b), nil
}

// subtractValues computes a - b for numeric types.
// Follows MongoDB promotion rules: float64 dominates, then int64, then int32.
func subtractValues(a, b any) any {
	switch av := a.(type) {
	case float64:
		return av - toFloat64(b)
	case int64:
		switch bv := b.(type) {
		case float64:
			return float64(av) - bv
		case int64:
			return av - bv
		case int32:
			return av - int64(bv)
		}
	case int32:
		switch bv := b.(type) {
		case float64:
			return float64(av) - bv
		case int64:
			return int64(av) - bv
		case int32:
			result := int64(av) - int64(bv)
			if result >= -1<<31 && result <= 1<<31-1 {
				return int32(result)
			}

			return result
		}
	}

	// fallback: convert both to float64
	return toFloat64(a) - toFloat64(b)
}

// -- $divide ------------------------------------------------------------------

// divideOp represents the $divide expression operator.
//
//	{ $divide: [ <expression1>, <expression2> ] }
type divideOp struct {
	dividend any
	divisor  any
}

// newDivide creates a new $divide operator.
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

// Process implements Operator. Returns dividend / divisor as float64.
// Returns null if either arg is null.
func (d *divideOp) Process(doc *types.Document) (any, error) {
	a, err := evalArgValue(d.dividend, doc)
	if err != nil {
		return nil, err
	}

	b, err := evalArgValue(d.divisor, doc)
	if err != nil {
		return nil, err
	}

	if a == types.Null || b == types.Null {
		return types.Null, nil
	}

	bf := toFloat64(b)
	if bf == 0 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$divide",
			"$divide only supports numeric types, not null",
		)
	}

	return toFloat64(a) / bf, nil
}

// toFloat64 converts a numeric value to float64.
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

// check interfaces
var (
	_ Operator = (*subtractOp)(nil)
	_ Operator = (*divideOp)(nil)
)
