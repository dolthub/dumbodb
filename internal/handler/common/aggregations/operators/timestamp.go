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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// tsSecondOp implements $tsSecond  -- returns the seconds component (high 32 bits) of a Timestamp.
type tsSecondOp struct {
	param any
}

// tsIncrementOp implements $tsIncrement  -- returns the increment/ordinal component (low 32 bits) of a Timestamp.
type tsIncrementOp struct {
	param any
}

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
	val, missing, err := EvalOperand(param, doc)
	if err != nil {
		return tsResult{}, err
	}
	if missing || isNullish(val) {
		return tsResult{missing: true}, nil
	}
	ts, ok := val.(types.Timestamp)
	if !ok {
		code := handlererrors.ErrTsSecondNotTimestamp
		if opName == "$tsIncrement" {
			code = handlererrors.ErrTsIncrementNotTimestamp
		}
		return tsResult{}, handlererrors.NewCommandErrorMsgWithArgument(code,
			fmt.Sprintf(" Argument to %s must be a timestamp, but is %s", opName, handlerparams.AliasFromType(val)), opName)
	}
	return tsResult{ts: ts}, nil
}

// Process implements Operator for $tsSecond.
// Returns the seconds component (high 32 bits) of the Timestamp as int64,
// or null when the input is missing or null.
func (t *tsSecondOp) Process(doc *types.Document) (any, error) {
	res, err := evaluateTimestampParam("$tsSecond", t.param, doc)
	if err != nil {
		return nil, err
	}

	if res.missing {
		return types.Null, nil
	}

	// Timestamp stores seconds in the high 32 bits.
	return int64(uint64(res.ts) >> 32), nil
}

// Process implements Operator for $tsIncrement.
// Returns the increment/ordinal component (low 32 bits) of the Timestamp as int64,
// or null when the input is missing or null.
func (t *tsIncrementOp) Process(doc *types.Document) (any, error) {
	res, err := evaluateTimestampParam("$tsIncrement", t.param, doc)
	if err != nil {
		return nil, err
	}

	if res.missing {
		return types.Null, nil
	}

	// Timestamp stores the ordinal/increment in the low 32 bits.
	return int64(uint64(res.ts) & 0xFFFFFFFF), nil
}

var (
	_ Operator = (*tsSecondOp)(nil)
	_ Operator = (*tsIncrementOp)(nil)
)
