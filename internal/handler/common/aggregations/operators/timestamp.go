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

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
)

// tsSecondOp implements $tsSecond — returns the seconds component (high 32 bits) of a Timestamp.
type tsSecondOp struct {
	param any
}

// tsIncrementOp implements $tsIncrement — returns the increment/ordinal component (low 32 bits) of a Timestamp.
type tsIncrementOp struct {
	param any
}

// newTsSecond creates a $tsSecond operator.
func newTsSecond(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$tsSecond",
			fmt.Sprintf("Expression $tsSecond takes exactly 1 arguments. %d were passed in.", len(args)),
		)
	}

	return &tsSecondOp{param: args[0]}, nil
}

// newTsIncrement creates a $tsIncrement operator.
func newTsIncrement(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$tsIncrement",
			fmt.Sprintf("Expression $tsIncrement takes exactly 1 arguments. %d were passed in.", len(args)),
		)
	}

	return &tsIncrementOp{param: args[0]}, nil
}

// tsResult wraps a Timestamp value and a missing flag.
// missing is true when the input field was absent (produces nil output without error).
type tsResult struct {
	ts      types.Timestamp
	missing bool
}

// evaluateTimestampParam resolves the operator parameter.
// Returns (result, nil) on success. When a field path resolves to a missing
// field, result.missing is true and the caller should return nil without error.
// Returns an error only when the argument is definitively not a Timestamp.
func evaluateTimestampParam(opName string, param any, doc *types.Document) (tsResult, error) {
	switch p := param.(type) {
	case string:
		if len(p) > 0 && p[0] == '$' {
			expr, err := aggregations.NewExpression(p, nil)
			if err != nil {
				return tsResult{}, err
			}

			val, err := expr.Evaluate(doc)
			if err != nil {
				// Field is absent — return null without error (matches MongoDB behaviour).
				return tsResult{missing: true}, nil
			}

			ts, ok := val.(types.Timestamp)
			if !ok {
				return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf("%s requires a timestamp argument, got %T", opName, val),
					opName,
				)
			}

			return tsResult{ts: ts}, nil
		}

		return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%s requires a timestamp argument, got string %q", opName, p),
			opName,
		)

	case types.Timestamp:
		return tsResult{ts: p}, nil

	case *types.Document:
		if !IsOperator(p) {
			return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("%s requires a timestamp argument, got object", opName),
				opName,
			)
		}

		op, err := NewOperator(p)
		if err != nil {
			return tsResult{}, err
		}

		val, err := op.Process(doc)
		if err != nil {
			return tsResult{}, err
		}

		if val == nil {
			return tsResult{missing: true}, nil
		}

		ts, ok := val.(types.Timestamp)
		if !ok {
			return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("%s requires a timestamp argument, got %T", opName, val),
				opName,
			)
		}

		return tsResult{ts: ts}, nil

	default:
		return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%s requires a timestamp argument, got %T", opName, param),
			opName,
		)
	}
}

// Process implements Operator for $tsSecond.
// Returns the seconds component (high 32 bits) of the Timestamp as int64,
// or nil when the input field is absent.
func (t *tsSecondOp) Process(doc *types.Document) (any, error) {
	res, err := evaluateTimestampParam("$tsSecond", t.param, doc)
	if err != nil {
		return nil, err
	}

	if res.missing {
		return nil, nil
	}

	// Timestamp stores seconds in the high 32 bits.
	return int64(uint64(res.ts) >> 32), nil
}

// Process implements Operator for $tsIncrement.
// Returns the increment/ordinal component (low 32 bits) of the Timestamp as int64,
// or nil when the input field is absent.
func (t *tsIncrementOp) Process(doc *types.Document) (any, error) {
	res, err := evaluateTimestampParam("$tsIncrement", t.param, doc)
	if err != nil {
		return nil, err
	}

	if res.missing {
		return nil, nil
	}

	// Timestamp stores the ordinal/increment in the low 32 bits.
	return int64(uint64(res.ts) & 0xFFFFFFFF), nil
}

// check interfaces
var (
	_ Operator = (*tsSecondOp)(nil)
	_ Operator = (*tsIncrementOp)(nil)
)
