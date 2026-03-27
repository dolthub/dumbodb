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

	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/must"
)

// ── $cond ────────────────────────────────────────────────────────────────────

// condOp represents the $cond expression operator.
//
//	// Array form:
//	{ $cond: [ <if-expr>, <then-expr>, <else-expr> ] }
//
//	// Document form:
//	{ $cond: { if: <if-expr>, then: <then-expr>, else: <else-expr> } }
type condOp struct {
	ifArg   any
	thenArg any
	elseArg any
}

// newCond creates a new $cond operator.
// Accepts either 3 positional args (array form) or 1 document arg (document form).
func newCond(args ...any) (Operator, error) {
	switch len(args) {
	case 3:
		// array form: [if, then, else]
		return &condOp{ifArg: args[0], thenArg: args[1], elseArg: args[2]}, nil

	case 1:
		doc, ok := args[0].(*types.Document)
		if !ok {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$cond",
				"$cond requires an object or array with three elements.",
			)
		}

		ifExpr, err := doc.Get("if")
		if err != nil {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$cond",
				"Missing 'if' parameter to $cond",
			)
		}

		thenExpr, err := doc.Get("then")
		if err != nil {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$cond",
				"Missing 'then' parameter to $cond",
			)
		}

		elseExpr, err := doc.Get("else")
		if err != nil {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				"$cond",
				"Missing 'else' parameter to $cond",
			)
		}

		return &condOp{ifArg: ifExpr, thenArg: thenExpr, elseArg: elseExpr}, nil

	default:
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$cond",
			fmt.Sprintf("$cond requires an object or array with three elements. Got %d.", len(args)),
		)
	}
}

// Process implements Operator.
// Evaluates ifArg; if truthy, returns thenArg result; otherwise elseArg result.
// MongoDB-falsy values: false and null; everything else is truthy (including 0).
func (c *condOp) Process(doc *types.Document) (any, error) {
	ifResult, err := evalArgValue(c.ifArg, doc)
	if err != nil {
		return nil, err
	}

	if isFalsy(ifResult) {
		return evalArgValue(c.elseArg, doc)
	}

	return evalArgValue(c.thenArg, doc)
}

// check interfaces
var _ Operator = (*condOp)(nil)

// ── $ifNull ───────────────────────────────────────────────────────────────────

// ifNullOp represents the $ifNull expression operator.
//
//	{ $ifNull: [ <input-expr>, <replacement-expr-if-null>, ... ] }
//
// Returns the first non-null evaluated argument.
// If all arguments are null, returns null.
type ifNullOp struct {
	args []any
}

// newIfNull creates a new $ifNull operator.
func newIfNull(args ...any) (Operator, error) {
	if len(args) < 2 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$ifNull",
			fmt.Sprintf("$ifNull needs at least two arguments, had: %d", len(args)),
		)
	}

	return &ifNullOp{args: args}, nil
}

// Process implements Operator.
// Returns the first argument that does not evaluate to null/missing.
// Falls back to the last argument if all are null.
func (n *ifNullOp) Process(doc *types.Document) (any, error) {
	for i, arg := range n.args {
		v, err := evalArgValue(arg, doc)
		if err != nil {
			return nil, err
		}

		// Return the first non-null value OR always return the last argument.
		if v != types.Null || i == len(n.args)-1 {
			return v, nil
		}
	}

	return must.NotFail(types.NewDocument()), nil // unreachable but satisfies compiler
}

// check interfaces
var _ Operator = (*ifNullOp)(nil)
