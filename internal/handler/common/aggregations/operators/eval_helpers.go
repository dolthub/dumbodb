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
	"errors"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// EvalArgValue is the exported view of evalArgValue, for callers outside this
// package that need to evaluate aggregation-style expressions (e.g. find
// projection resolving "$$ROOT" and "$field" expressions).
func EvalArgValue(arg any, doc *types.Document) (any, error) {
	return evalArgValue(arg, doc)
}

// evalArgValue resolves an operator argument to its concrete value against the document.
// Supports:
//   - *types.Document with operator -> evaluates nested operator
//   - *types.Document without operator -> evaluates each value as an expression
//   - "$$ROOT" / "$$CURRENT" -> the current document (with optional ".field" traversal)
//   - other "$$name" -> variable bound by $filter/$map/$let in the doc
//   - "$path" -> field-path expression
//   - other string -> literal
//   - any other value -> literal value
func evalArgValue(arg any, doc *types.Document) (any, error) {
	switch v := arg.(type) {
	case *types.Document:
		if IsOperator(v) {
			op, err := NewOperator(v)
			if err != nil {
				return nil, err
			}

			return op.Process(doc)
		}

		// Non-operator document: treat as an expression object  -- each value is an expression.
		return evalDocumentExpressions(v, doc)

	case string:
		if strings.HasPrefix(v, "$$") {
			withoutPrefix := strings.TrimPrefix(v, "$$")
			varName := withoutPrefix
			fieldPath := ""
			if dotIdx := strings.Index(withoutPrefix, "."); dotIdx >= 0 {
				varName = withoutPrefix[:dotIdx]
				fieldPath = withoutPrefix[dotIdx+1:]
			}

			if varName == "" {
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrFailedToParse,
					"empty variable names are not allowed",
				)
			}

			var base any
			switch varName {
			case "ROOT", "CURRENT":
				base = doc.DeepCopy()
			default:
				val, err := doc.Get("$$" + varName)
				if err != nil {
					if fieldPath != "" {
						return types.Null, nil
					}
					return v, nil
				}
				base = val
			}

			if fieldPath == "" {
				return base, nil
			}

			baseDoc, ok := base.(*types.Document)
			if !ok {
				return types.Null, nil
			}

			path, err := types.NewPathFromString(fieldPath)
			if err != nil {
				return types.Null, nil
			}

			result, err := baseDoc.GetByPath(path)
			if err != nil {
				return types.Null, nil
			}

			return result, nil
		}

		if strings.HasPrefix(v, "$") {
			expr, err := aggregations.NewExpression(v, nil)
			if err != nil {
				var exErr *aggregations.ExpressionError
				if errors.As(err, &exErr) && exErr.Code() == aggregations.ErrNotExpression {
					return v, nil
				}

				return nil, err
			}

			val, err := expr.Evaluate(doc)
			if err != nil {
				// missing field evaluates to null
				return types.Null, nil
			}

			return val, nil
		}

		return v, nil

	default:
		return v, nil
	}
}

// evalDocumentExpressions evaluates each value of a non-operator expression document as an
// expression against doc, returning a new document with the resolved values. This matches
// MongoDB's behavior where `{key: "$field"}` inside an expression context produces
// `{key: <value of $field>}`.
func evalDocumentExpressions(expr *types.Document, doc *types.Document) (*types.Document, error) {
	result := must.NotFail(types.NewDocument())

	for _, k := range expr.Keys() {
		val := must.NotFail(expr.Get(k))

		evaluated, err := evalArgValue(val, doc)
		if err != nil {
			return nil, err
		}

		result.Set(k, evaluated)
	}

	return result, nil
}

// isFalsy returns true when v is MongoDB-falsy for $cond purposes:
// false (bool), null, or missing.
func isFalsy(v any) bool {
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
