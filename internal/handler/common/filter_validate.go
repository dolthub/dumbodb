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
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ValidateFilter returns the error MongoDB raises when it parses filter, which
// evaluation alone misses when no document reaches the invalid predicate. Each
// predicate is evaluated on its own against an empty document, so one that
// does not match cannot hide a later invalid one. $expr is not evaluated, as
// MongoDB raises its errors per document, but its constants are folded.
func ValidateFilter(filter *types.Document, cmp *collation.Comparator) error {
	empty := types.MakeDocument(0)
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$and", "$or", "$nor":
			if _, err := FilterDocumentColl(empty, must.NotFail(types.NewDocument(key, value)), cmp); err != nil {
				return err
			}
			branches, _ := value.(*types.Array)
			for i := 0; i < branches.Len(); i++ {
				if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
					if err := ValidateFilter(branch, cmp); err != nil {
						return err
					}
				}
			}
		case "$expr":
			if err := operators.CheckConstantSubexpressions(value); err != nil {
				return err
			}
		case "$where", "$text", "$comment":
		default:
			if err := validateFieldPredicate(empty, key, value, cmp); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFieldPredicate(empty *types.Document, key string, value any, cmp *collation.Comparator) error {
	expr, ok := value.(*types.Document)
	if !ok || expr.Len() == 0 || !strings.HasPrefix(expr.Keys()[0], "$") {
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
			if err := ValidateFilter(inner, cmp); err != nil {
				return err
			}
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
	switch first := doc.Keys()[0]; first {
	case "$and", "$or", "$nor", "$expr", "$where", "$text", "$comment":
		return false
	default:
		return strings.HasPrefix(first, "$")
	}
}

// CheckExprConstants returns the error MongoDB raises while parsing when a
// constant folded in one of filter's $expr fails.
func CheckExprConstants(filter *types.Document) error {
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$expr":
			if err := operators.CheckConstantSubexpressions(value); err != nil {
				return err
			}
		case "$and", "$or", "$nor":
			branches, ok := value.(*types.Array)
			if !ok {
				continue
			}
			for i := range branches.Len() {
				if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
					if err := CheckExprConstants(branch); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
