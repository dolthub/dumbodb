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
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// literalOp represents { $literal: <value> }.
// Returns the value without evaluating it as an expression.
type literalOp struct{ val any }

func newLiteral(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$literal",
			fmt.Sprintf("Expression $literal takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &literalOp{val: args[0]}, nil
}

func (op *literalOp) Process(_ *types.Document) (any, error) {
	return op.val, nil
}

var _ Operator = (*literalOp)(nil)

// letOp represents { $let: { vars: { <name>: <expr>, ... }, in: <expr> } }.
// Binds variables (as $$name keys in a temporary document) and evaluates `in`.
type letOp struct {
	varsDoc *types.Document // raw vars document
	inExpr  any
}

func newLet(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$let",
			"$let requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$let",
			"$let requires a document argument")
	}

	varsVal, err := doc.Get("vars")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$let",
			"Missing 'vars' parameter to $let")
	}

	varsDoc, ok := varsVal.(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$let",
			"'vars' must be a document in $let")
	}

	inExpr, err := doc.Get("in")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$let",
			"Missing 'in' parameter to $let")
	}

	return &letOp{varsDoc: varsDoc, inExpr: inExpr}, nil
}

func (op *letOp) Process(doc *types.Document) (any, error) {
	// Build an augmented document that includes the evaluated variables as $$name.
	augmented := doc.DeepCopy()

	for _, varName := range op.varsDoc.Keys() {
		varExpr := must.NotFail(op.varsDoc.Get(varName))

		val, err := evalArgValue(varExpr, doc)
		if err != nil {
			return nil, err
		}

		augmented.Set("$$"+varName, val)
	}

	return evalArgValue(op.inExpr, augmented)
}

var _ Operator = (*letOp)(nil)

// isNumberOp represents { $isNumber: <expr> }.
// Returns true if the value is a numeric type (double, int, long, decimal).
type isNumberOp struct{ arg any }

func newIsNumber(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$isNumber",
			fmt.Sprintf("Expression $isNumber takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &isNumberOp{arg: args[0]}, nil
}

func (op *isNumberOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	switch v.(type) {
	case float64, int32, int64:
		return true, nil
	default:
		return false, nil
	}
}

var _ Operator = (*isNumberOp)(nil)

type isStringOp struct{ arg any }

func newIsString(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$isString",
			fmt.Sprintf("Expression $isString takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &isStringOp{arg: args[0]}, nil
}

func (op *isStringOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	_, ok := v.(string)

	return ok, nil
}

var _ Operator = (*isStringOp)(nil)

type isObjectIdOp struct{ arg any }

func newIsObjectId(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$isObjectId",
			fmt.Sprintf("Expression $isObjectId takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &isObjectIdOp{arg: args[0]}, nil
}

func (op *isObjectIdOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	_, ok := v.(types.ObjectID)

	return ok, nil
}

var _ Operator = (*isObjectIdOp)(nil)

type isDateOp struct{ arg any }

func newIsDate(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$isDate",
			fmt.Sprintf("Expression $isDate takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &isDateOp{arg: args[0]}, nil
}

func (op *isDateOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	_, ok := v.(time.Time)

	return ok, nil
}

var _ Operator = (*isDateOp)(nil)
