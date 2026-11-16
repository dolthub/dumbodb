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

package common

import (
	"fmt"
	"math"
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ValidateFilter returns the error MongoDB raises when it parses filter, which
// evaluation alone misses when no document reaches the invalid predicate. Each
// predicate is evaluated on its own against an empty document, so one that
// does not match cannot hide a later invalid one. $expr is not evaluated, as
// MongoDB raises its errors per document, but its constants are folded.
func ValidateFilter(filter *types.Document, cmp *collation.Comparator) error {
	if err := checkNearPlacement(filter, true); err != nil {
		return err
	}
	return validateFilter(filter, cmp, true)
}

// ValidateFilterNoGeoNear is ValidateFilter for a context that refuses $near,
// $nearSphere and $geoNear anywhere in a query, such as $match and validators:
// mongod reports 5626500 there for any well-formed one, wherever it is.
func ValidateFilterNoGeoNear(filter *types.Document, cmp *collation.Comparator) error {
	if err := validateFilter(filter, cmp, true); err != nil {
		return err
	}
	return refuseGeoNear(filter)
}

func refuseGeoNear(filter *types.Document) error {
	if checkNearPlacement(filter, false) != nil {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrGeoNearNotAllowedInContext, GeoNearNotAllowedMsg)
	}
	return nil
}

// checkNearPlacement refuses $near, $nearSphere and $geoNear outside the top
// level of a query: mongod accepts them there, in $and and in an $or of one
// clause, which it folds away. topLevel says whether query is in such a place.
func checkNearPlacement(query *types.Document, topLevel bool) error {
	for _, key := range query.Keys() {
		value := must.NotFail(query.Get(key))
		switch key {
		case "$and", "$or", "$nor":
			clauses, ok := value.(*types.Array)
			if !ok {
				continue
			}
			inner := topLevel && (key == "$and" || key == "$or" && clauses.Len() == 1)
			for i := range clauses.Len() {
				if clause, ok := must.NotFail(clauses.Get(i)).(*types.Document); ok {
					if err := checkNearPlacement(clause, inner); err != nil {
						return err
					}
				}
			}
		default:
			if ops, ok := value.(*types.Document); ok && !strings.HasPrefix(key, "$") && isOperatorDocument(ops) {
				if err := checkNearOperators(ops, topLevel); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkNearOperators is checkNearPlacement for the operators of one field,
// where $not, $elemMatch and the $elemMatch operands of $all are never the
// top level.
func checkNearOperators(ops *types.Document, topLevel bool) error {
	for _, op := range ops.Keys() {
		operand := must.NotFail(ops.Get(op))
		switch op {
		case "$near", "$nearSphere", "$geoNear":
			if !topLevel {
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue, "geo $near must be top-level expr", op,
				)
			}
		case "$not":
			if inner, ok := operand.(*types.Document); ok && isOperatorDocument(inner) {
				if err := checkNearOperators(inner, false); err != nil {
					return err
				}
			}
		case "$elemMatch":
			if err := checkNearInElemMatch(operand); err != nil {
				return err
			}
		case "$all":
			elems, ok := operand.(*types.Array)
			if !ok {
				continue
			}
			for i := range elems.Len() {
				if elem, ok := must.NotFail(elems.Get(i)).(*types.Document); ok && isElemMatchDocument(elem) {
					if err := checkNearInElemMatch(must.NotFail(elem.Get("$elemMatch"))); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func checkNearInElemMatch(operand any) error {
	inner, ok := operand.(*types.Document)
	if !ok {
		return nil
	}
	if isFieldOperatorExpression(inner) {
		return checkNearOperators(inner, false)
	}
	return checkNearPlacement(inner, false)
}

// validateFilter is ValidateFilter; foldExpr says whether $expr constants are
// folded, which MongoDB does for queries but not for collection validators.
func validateFilter(filter *types.Document, cmp *collation.Comparator, foldExpr bool) error {
	empty := types.MakeDocument(0)
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$and", "$or", "$nor":
			// $expr is checked below, not evaluated: its errors depend on
			// the documents, as in mongod.
			if _, err := FilterDocumentColl(empty, must.NotFail(types.NewDocument(key, withoutExpr(value))), cmp); err != nil {
				return err
			}
			branches, _ := value.(*types.Array)
			for i := 0; i < branches.Len(); i++ {
				if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
					if err := validateFilter(branch, cmp, foldExpr); err != nil {
						return err
					}
				}
			}
		case "$expr":
			if err := operators.CheckFieldPaths(value); err != nil {
				return err
			}
			if !foldExpr {
				continue
			}
			if err := operators.CheckConstantSubexpressions(value); err != nil {
				return err
			}
		case "$where", "$text", "$comment":
		case "$alwaysTrue", "$alwaysFalse":
			if err := checkAlwaysOperand(key, value); err != nil {
				return err
			}
		default:
			if err := validateFieldPredicate(empty, key, value, cmp); err != nil {
				return err
			}
		}
	}
	return nil
}

// withoutExpr returns the clauses of a logical operator with their $expr
// conditions removed.
func withoutExpr(v any) any {
	clauses, ok := v.(*types.Array)
	if !ok {
		return v
	}
	out := types.MakeArray(clauses.Len())
	for i := range clauses.Len() {
		clause := must.NotFail(clauses.Get(i))
		if d, ok := clause.(*types.Document); ok {
			stripped := types.MakeDocument(d.Len())
			for _, k := range d.Keys() {
				switch k {
				case "$expr":
				case "$and", "$or", "$nor":
					stripped.Set(k, withoutExpr(must.NotFail(d.Get(k))))
				default:
					stripped.Set(k, must.NotFail(d.Get(k)))
				}
			}
			clause = stripped
		}
		out.Append(clause)
	}
	return out
}

func validateFieldPredicate(empty *types.Document, key string, value any, cmp *collation.Comparator) error {
	expr, ok := value.(*types.Document)
	if !ok || expr.Len() == 0 || !strings.HasPrefix(expr.Command(), "$") || isDBRefLiteral(expr) {
		_, err := FilterDocumentColl(empty, must.NotFail(types.NewDocument(key, value)), cmp)
		return err
	}
	for _, op := range expr.Keys() {
		if op == "$options" {
			continue
		}
		operand := must.NotFail(expr.Get(op))
		single := must.NotFail(types.NewDocument(op, operand))
		if options, err := expr.Get("$options"); err == nil && op == "$regex" {
			single.Set("$options", options)
		}
		if _, err := FilterDocumentColl(empty, must.NotFail(types.NewDocument(key, single)), cmp); err != nil {
			return err
		}
		if op == "$all" {
			if err := validateAllElemMatches(empty, key, operand, cmp); err != nil {
				return err
			}
		}
		inner, isDoc := operand.(*types.Document)
		switch {
		case !isDoc:
		case op == "$not":
			if err := validateFieldPredicate(empty, key, inner, cmp); err != nil {
				return err
			}
		case op == "$elemMatch" && isFieldOperatorExpression(inner):
			if err := validateFieldPredicate(empty, key, inner, cmp); err != nil {
				return err
			}
		case op == "$elemMatch":
			if err := checkElemMatchTopLevelOnly(inner); err != nil {
				return err
			}
			if err := ValidateFilter(inner, cmp); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAllElemMatches validates the $elemMatch operands of an $all, which
// evaluation reaches only for a document holding an array at key.
func validateAllElemMatches(empty *types.Document, key string, operand any, cmp *collation.Comparator) error {
	elems, ok := operand.(*types.Array)
	if !ok {
		return nil
	}
	for i := range elems.Len() {
		elem, ok := must.NotFail(elems.Get(i)).(*types.Document)
		if !ok || !isElemMatchDocument(elem) {
			continue
		}
		single := must.NotFail(types.NewDocument("$elemMatch", must.NotFail(elem.Get("$elemMatch"))))
		if err := validateFieldPredicate(empty, key, single, cmp); err != nil {
			return err
		}
	}
	return nil
}

// isFieldOperatorExpression reports whether an $elemMatch operand applies
// operators to each element, like {$gt: 1}, rather than a query to each
// element document, like {a: 1} or {$or: [...]}.
func isFieldOperatorExpression(doc *types.Document) bool {
	if doc.Len() == 0 {
		return false
	}
	switch first := doc.Command(); first {
	case "$and", "$or", "$nor", "$expr", "$where", "$text", "$comment", "$alwaysTrue", "$alwaysFalse":
		return false
	default:
		return strings.HasPrefix(first, "$")
	}
}

// ValidateFilterExprs returns the error MongoDB raises while parsing each
// $expr in filter: a malformed expression or a failing folded constant.
func ValidateFilterExprs(filter *types.Document) error {
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$expr":
			if _, err := validatedExpr(value); err != nil {
				return err
			}
		case "$and", "$or", "$nor":
			branches, ok := value.(*types.Array)
			if !ok {
				continue
			}
			for i := range branches.Len() {
				if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
					if err := ValidateFilterExprs(branch); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// checkAlwaysOperand returns mongod's error for an $alwaysTrue or
// $alwaysFalse operand, which must be the number 1.
func checkAlwaysOperand(op string, v any) error {
	var n int64
	switch v := v.(type) {
	case int32:
		n = int64(v)
	case int64:
		n = v
	case float64:
		switch {
		case math.IsNaN(v):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				fmt.Sprintf("Expected an integer, but found NaN in: %s: %s", op, types.FormatAnyValue(v)), op)
		case v != math.Trunc(v):
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				fmt.Sprintf("Expected an integer: %s: %s", op, types.FormatAnyValue(v)), op)
		case v < math.MinInt64 || v >= math.MaxInt64:
			return errNotInt64(op, v)
		}
		n = int64(v)
	case types.Decimal128:
		var ok bool
		if n, ok = decimal128ToInt64(v); !ok {
			return errNotInt64(op, v)
		}
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			fmt.Sprintf("Expected a number in: %s: %s", op, types.FormatAnyValue(v)), op)
	}
	if n != 1 {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			op+" must be an integer value of 1", op)
	}
	return nil
}

func errNotInt64(op string, v any) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
		fmt.Sprintf("Cannot represent as a 64-bit integer: %s: %s", op, types.FormatAnyValue(v)), op)
}
