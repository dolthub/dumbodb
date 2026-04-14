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

// compOp is the type of comparison to perform.
type compOp int

const (
	compEq  compOp = iota // $eq
	compNe                // $ne
	compGt                // $gt
	compGte               // $gte
	compLt                // $lt
	compLte               // $lte
)

// cmpOperator represents $eq, $ne, $gt, $gte, $lt, $lte expression operators.
//
//	{ $eq:  [ <expr1>, <expr2> ] }
//	{ $ne:  [ <expr1>, <expr2> ] }
//	{ $gt:  [ <expr1>, <expr2> ] }
//	{ $gte: [ <expr1>, <expr2> ] }
//	{ $lt:  [ <expr1>, <expr2> ] }
//	{ $lte: [ <expr1>, <expr2> ] }
type cmpOperator struct {
	left  any
	right any
	op    compOp
	name  string
}

// newCmpOperator creates a new comparison operator.
func newCmpOperator(name string, op compOp) newOperatorFunc {
	return func(args ...any) (Operator, error) {
		if len(args) != 2 {
			return nil, newOperatorError(
				ErrArgsInvalidLen,
				name,
				fmt.Sprintf("Expression %s takes exactly 2 arguments. %d were passed in.", name, len(args)),
			)
		}

		return &cmpOperator{left: args[0], right: args[1], op: op, name: name}, nil
	}
}

// Process implements Operator. Evaluates both args and performs the comparison.
func (c *cmpOperator) Process(doc *types.Document) (any, error) {
	lv, err := evalArgValue(c.left, doc)
	if err != nil {
		return nil, err
	}

	rv, err := evalArgValue(c.right, doc)
	if err != nil {
		return nil, err
	}

	cmp := types.Compare(lv, rv)

	switch c.op {
	case compEq:
		return cmp == types.Equal, nil
	case compNe:
		return cmp != types.Equal, nil
	case compGt:
		return cmp == types.Greater, nil
	case compGte:
		return cmp == types.Greater || cmp == types.Equal, nil
	case compLt:
		return cmp == types.Less, nil
	case compLte:
		return cmp == types.Less || cmp == types.Equal, nil
	default:
		return false, nil
	}
}

// check interfaces
var _ Operator = (*cmpOperator)(nil)

// ── $cmp ─────────────────────────────────────────────────────────────────────

// cmpOp represents { $cmp: [ <expr1>, <expr2> ] }.
// Returns -1 if expr1 < expr2, 0 if equal, 1 if expr1 > expr2.
type cmpOp struct {
	left  any
	right any
}

// newCmp creates a new $cmp operator.
func newCmp(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$cmp",
			fmt.Sprintf("Expression $cmp takes exactly 2 arguments. %d were passed in.", len(args)),
		)
	}

	return &cmpOp{left: args[0], right: args[1]}, nil
}

// Process implements Operator.
func (c *cmpOp) Process(doc *types.Document) (any, error) {
	lv, err := evalArgValue(c.left, doc)
	if err != nil {
		return nil, err
	}

	rv, err := evalArgValue(c.right, doc)
	if err != nil {
		return nil, err
	}

	switch types.Compare(lv, rv) {
	case types.Less:
		return int32(-1), nil
	case types.Equal:
		return int32(0), nil
	default:
		return int32(1), nil
	}
}

// check interfaces
var _ Operator = (*cmpOp)(nil)
