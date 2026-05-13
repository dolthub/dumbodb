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

	"github.com/dolthub/dumbodb/internal/types"
)

// isBoolFalsy returns true when v is falsy in MongoDB aggregation boolean context:
// false (bool) or null. All other values  -- including 0, empty string, empty array  -- are truthy.
func isBoolFalsy(v any) bool {
	switch val := v.(type) {
	case bool:
		return !val
	case types.NullType:
		return true
	default:
		_ = val
		return false
	}
}

// andOp represents { $and: [ <expr1>, <expr2>, ... ] }.
// Returns true if all expressions are truthy (non-false, non-null).
type andOp struct{ args []any }

func newAnd(args ...any) (Operator, error) {
	return &andOp{args: args}, nil
}

func (op *andOp) Process(doc *types.Document) (any, error) {
	for _, arg := range op.args {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}

		if isBoolFalsy(v) {
			return false, nil
		}
	}

	return true, nil
}

var _ Operator = (*andOp)(nil)

// orOp represents { $or: [ <expr1>, <expr2>, ... ] }.
// Returns true if any expression is truthy.
type orOp struct{ args []any }

func newOr(args ...any) (Operator, error) {
	return &orOp{args: args}, nil
}

func (op *orOp) Process(doc *types.Document) (any, error) {
	for _, arg := range op.args {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}

		if !isBoolFalsy(v) {
			return true, nil
		}
	}

	return false, nil
}

var _ Operator = (*orOp)(nil)

// notOp represents { $not: [ <expr> ] }.
// Returns true if the single expression is falsy.
type notOp struct{ arg any }

func newNot(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$not",
			fmt.Sprintf("Expression $not takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &notOp{arg: args[0]}, nil
}

func (op *notOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	return isBoolFalsy(v), nil
}

var _ Operator = (*notOp)(nil)
