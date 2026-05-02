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
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
)

// -- $concat -------------------------------------------------------------------

// concatOp represents the $concat expression operator.
//
//	{ $concat: [ <expression1>, <expression2>, ... ] }
type concatOp struct {
	args []any
}

// newConcat creates a new $concat operator.
func newConcat(args ...any) (Operator, error) {
	return &concatOp{args: args}, nil
}

// Process implements Operator. Evaluates each arg and concatenates the string results.
// Returns null if any arg evaluates to null.
func (c *concatOp) Process(doc *types.Document) (any, error) {
	var buf strings.Builder

	for _, arg := range c.args {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}

		if v == types.Null {
			return types.Null, nil
		}

		s, ok := v.(string)
		if !ok {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$concat",
				fmt.Sprintf("$concat only supports strings, not %T", v),
			)
		}

		buf.WriteString(s)
	}

	return buf.String(), nil
}

// check interfaces
var _ Operator = (*concatOp)(nil)

// -- $toLower ------------------------------------------------------------------

// toLowerOp represents the $toLower expression operator.
//
//	{ $toLower: <expression> }
type toLowerOp struct {
	arg any
}

// newToLower creates a new $toLower operator.
func newToLower(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toLower",
			fmt.Sprintf("Expression $toLower takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toLowerOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to lower case.
// Returns null if arg evaluates to null.
func (op *toLowerOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	s, ok := v.(string)
	if !ok {
		// non-string non-null: MongoDB returns ""
		return "", nil
	}

	return strings.ToLower(s), nil
}

// check interfaces
var _ Operator = (*toLowerOp)(nil)

// -- $toUpper ------------------------------------------------------------------

// toUpperOp represents the $toUpper expression operator.
//
//	{ $toUpper: <expression> }
type toUpperOp struct {
	arg any
}

// newToUpper creates a new $toUpper operator.
func newToUpper(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$toUpper",
			fmt.Sprintf("Expression $toUpper takes exactly 1 argument. %d were passed in.", len(args)),
		)
	}

	return &toUpperOp{arg: args[0]}, nil
}

// Process implements Operator. Converts the evaluated arg to upper case.
// Returns null if arg evaluates to null.
func (op *toUpperOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	s, ok := v.(string)
	if !ok {
		// non-string non-null: MongoDB returns ""
		return "", nil
	}

	return strings.ToUpper(s), nil
}

// check interfaces
var _ Operator = (*toUpperOp)(nil)
