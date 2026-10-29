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
	"github.com/dolthub/dumbodb/internal/util/must"
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
	cmp, err := compareArgs(c.left, c.right, doc)
	if err != nil {
		return nil, err
	}

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

var _ Operator = (*cmpOperator)(nil)

// cmpOp represents { $cmp: [ <expr1>, <expr2> ] }.
// Returns -1 if expr1 < expr2, 0 if equal, 1 if expr1 > expr2.
type cmpOp struct {
	left  any
	right any
}

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

func (c *cmpOp) Process(doc *types.Document) (any, error) {
	cmp, err := compareArgs(c.left, c.right, doc)
	if err != nil {
		return nil, err
	}

	switch cmp {
	case types.Less:
		return int32(-1), nil
	case types.Equal:
		return int32(0), nil
	default:
		return int32(1), nil
	}
}

var _ Operator = (*cmpOp)(nil)

// compareArgs evaluates and compares two operator arguments. As in MongoDB,
// a missing field equals another missing value or undefined and sorts below
// every other value except MinKey.
func compareArgs(left, right any, doc *types.Document) (types.CompareResult, error) {
	lv, lMissing, err := evalArgValueOrMissing(left, doc)
	if err != nil {
		return 0, err
	}
	rv, rMissing, err := evalArgValueOrMissing(right, doc)
	if err != nil {
		return 0, err
	}
	if lMissing || rMissing {
		lLow := lMissing || lv == types.Undefined
		rLow := rMissing || rv == types.Undefined
		switch {
		case lLow && rLow:
			return types.Equal, nil
		case lLow && rv == types.MinKey, rLow && lv != types.MinKey:
			return types.Greater, nil
		default:
			return types.Less, nil
		}
	}
	return types.Compare(lv, rv), nil
}

// evalArgValueOrMissing is evalArgValue, also reporting whether arg is a
// field path the document lacks, which evaluates to null.
func evalArgValueOrMissing(arg any, doc *types.Document) (any, bool, error) {
	if path, ok := arg.(string); ok && strings.HasPrefix(path, "$$") {
		if v, missing, ok := variablePathOrMissing(path, doc); ok {
			return v, missing, nil
		}
	}
	if path, ok := arg.(string); ok && strings.HasPrefix(path, "$") && !strings.HasPrefix(path, "$$") {
		if expr, err := fieldExpression(path); err == nil {
			v, err := expr.Evaluate(doc)
			if err != nil {
				return types.Null, true, nil
			}
			return v, false, nil
		}
	}
	v, err := evalArgValue(arg, doc)
	return v, false, err
}

// variablePathOrMissing evaluates $$name.path when name is bound, as $let,
// $map and $filter bind variables, or is ROOT or CURRENT; ok is false
// otherwise. The path follows field path rules from the variable's value.
func variablePathOrMissing(path string, doc *types.Document) (value any, missing, ok bool) {
	name, sub, hasSub := strings.Cut(strings.TrimPrefix(path, "$$"), ".")
	if !hasSub || doc == nil {
		return nil, false, false
	}

	var base any = doc
	if name != "ROOT" && name != "CURRENT" {
		v, err := doc.Get("$$" + name)
		if err != nil {
			return nil, false, false
		}
		base = v
	}

	expr, err := fieldExpression("$v." + sub)
	if err != nil {
		return nil, false, false
	}
	v, err := expr.Evaluate(must.NotFail(types.NewDocument("v", base)))
	if err != nil {
		return types.Null, true, true
	}
	return v, false, true
}
