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
	"time"

	"github.com/dolthub/dongo/internal/types"
)

// ── $toInt ────────────────────────────────────────────────────────────────────

// toIntOp represents the $toInt expression operator.
//
//	{ $toInt: <expression> }
type toIntOp struct {
	arg any
}

// newToInt creates a new $toInt operator.
func newToInt(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toInt",
			fmt.Sprintf("Expression $toInt takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toIntOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to int32.
// Returns null if arg is null.
func (op *toIntOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case int32:
		return val, nil
	case int64:
		return int32(val), nil
	case float64:
		return int32(math.Trunc(val)), nil
	case bool:
		if val {
			return int32(1), nil
		}

		return int32(0), nil
	case string:
		n, err := strconv.ParseInt(val, 10, 32)
		if err != nil {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$toInt",
				fmt.Sprintf("Failed to parse number '%s' in $convert with no onError value", val),
			)
		}

		return int32(n), nil
	default:
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toInt",
			fmt.Sprintf("Unsupported conversion from %T in $toInt", v),
		)
	}
}

// check interfaces
var _ Operator = (*toIntOp)(nil)

// ── $toString ─────────────────────────────────────────────────────────────────

// toStringOp represents the $toString expression operator.
//
//	{ $toString: <expression> }
type toStringOp struct {
	arg any
}

// newToString creates a new $toString operator.
func newToString(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toString",
			fmt.Sprintf("Expression $toString takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toStringOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to string.
// Returns null if arg is null.
func (op *toStringOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case string:
		return val, nil
	case int32:
		return strconv.FormatInt(int64(val), 10), nil
	case int64:
		return strconv.FormatInt(val, 10), nil
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), nil
	case bool:
		if val {
			return "true", nil
		}

		return "false", nil
	case time.Time:
		return val.UTC().Format(time.RFC3339Nano), nil
	case types.ObjectID:
		return fmt.Sprintf("%x", [12]byte(val)), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// check interfaces
var _ Operator = (*toStringOp)(nil)

// ── $toDouble ─────────────────────────────────────────────────────────────────

// toDoubleOp represents the $toDouble expression operator.
//
//	{ $toDouble: <expression> }
type toDoubleOp struct {
	arg any
}

// newToDouble creates a new $toDouble operator.
func newToDouble(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toDouble",
			fmt.Sprintf("Expression $toDouble takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toDoubleOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to float64.
// Returns null if arg is null.
func (op *toDoubleOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case float64:
		return val, nil
	case int32:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case bool:
		if val {
			return 1.0, nil
		}

		return 0.0, nil
	case string:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$toDouble",
				fmt.Sprintf("Failed to parse number '%s' in $convert with no onError value", val),
			)
		}

		return n, nil
	default:
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toDouble",
			fmt.Sprintf("Unsupported conversion from %T in $toDouble", v),
		)
	}
}

// check interfaces
var _ Operator = (*toDoubleOp)(nil)

// ── $toDate ───────────────────────────────────────────────────────────────────

// toDateOp represents the $toDate expression operator.
//
//	{ $toDate: <expression> }
type toDateOp struct {
	arg any
}

// newToDate creates a new $toDate operator.
func newToDate(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toDate",
			fmt.Sprintf("Expression $toDate takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toDateOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to time.Time.
// Returns null if arg is null.
func (op *toDateOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case time.Time:
		return val, nil
	case int64:
		// milliseconds since epoch
		return time.Unix(0, val*int64(time.Millisecond)).UTC(), nil
	case int32:
		return time.Unix(0, int64(val)*int64(time.Millisecond)).UTC(), nil
	case float64:
		ms := int64(val)
		return time.Unix(0, ms*int64(time.Millisecond)).UTC(), nil
	case string:
		// try RFC3339 / ISO 8601 formats
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.999Z",
			"2006-01-02",
		} {
			if t, err := time.Parse(layout, val); err == nil {
				return t.UTC(), nil
			}
		}

		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toDate",
			fmt.Sprintf("Failed to parse date '%s' in $convert with no onError value", val),
		)
	default:
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toDate",
			fmt.Sprintf("Unsupported conversion from %T in $toDate", v),
		)
	}
}

// check interfaces
var _ Operator = (*toDateOp)(nil)

// ── $toLong ───────────────────────────────────────────────────────────────────

type toLongOp struct{ arg any }

func newToLong(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$toLong",
			fmt.Sprintf("Expression $toLong takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &toLongOp{arg: args[0]}, nil
}

func (op *toLongOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case int64:
		return val, nil
	case int32:
		return int64(val), nil
	case float64:
		return int64(math.Trunc(val)), nil
	case bool:
		if val {
			return int64(1), nil
		}

		return int64(0), nil
	case string:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$toLong",
				fmt.Sprintf("Failed to parse number '%s' in $convert with no onError value", val))
		}

		return n, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$toLong",
			fmt.Sprintf("Unsupported conversion from %T in $toLong", v))
	}
}

var _ Operator = (*toLongOp)(nil)

// ── $toDecimal ────────────────────────────────────────────────────────────────

// toDecimalOp converts to float64 (Dongo uses float64 for Decimal128 approximation).
type toDecimalOp struct{ arg any }

func newToDecimal(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$toDecimal",
			fmt.Sprintf("Expression $toDecimal takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &toDecimalOp{arg: args[0]}, nil
}

func (op *toDecimalOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case float64:
		return val, nil
	case int32:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case bool:
		if val {
			return 1.0, nil
		}

		return 0.0, nil
	case string:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$toDecimal",
				fmt.Sprintf("Failed to parse number '%s' in $convert with no onError value", val))
		}

		return n, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$toDecimal",
			fmt.Sprintf("Unsupported conversion from %T in $toDecimal", v))
	}
}

var _ Operator = (*toDecimalOp)(nil)

// ── $toBool ───────────────────────────────────────────────────────────────────

type toBoolOp struct{ arg any }

func newToBool(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$toBool",
			fmt.Sprintf("Expression $toBool takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &toBoolOp{arg: args[0]}, nil
}

func (op *toBoolOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	switch val := v.(type) {
	case bool:
		return val, nil
	case int32:
		return val != 0, nil
	case int64:
		return val != 0, nil
	case float64:
		return val != 0, nil
	case string:
		return val != "", nil
	default:
		// Documents, arrays, ObjectID, etc. are truthy.
		return true, nil
	}
}

var _ Operator = (*toBoolOp)(nil)
