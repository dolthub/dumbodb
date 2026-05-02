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
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// evalArgValue resolves an operator argument to its concrete value against the document.
// Supports:
//   - *types.Document with operator → evaluates nested operator
//   - *types.Document without operator → returns doc as-is
//   - string starting with "$$" → variable reference (looked up as "$$name" key in doc)
//   - string starting with "$" → field path expression
//   - string not starting with "$" → literal string value
//   - any other value → literal value
//
// Missing field paths return types.Null.
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
			// Variable reference: look up "$$name" key stored in the document by $filter/$map/$reduce/$let.
			// Handle dotted paths: "$$varname.field.sub" → resolve $$varname then traverse field.sub.
			withoutPrefix := strings.TrimPrefix(v, "$$")
			if dotIdx := strings.Index(withoutPrefix, "."); dotIdx >= 0 {
				varKey := "$$" + withoutPrefix[:dotIdx]
				fieldPath := withoutPrefix[dotIdx+1:]

				varVal, err := doc.Get(varKey)
				if err != nil {
					// Unknown variable  -- treat as a literal string.
					return v, nil
				}

				varDoc, ok := varVal.(*types.Document)
				if !ok {
					return types.Null, nil
				}

				path, err := types.NewPathFromString(fieldPath)
				if err != nil {
					return types.Null, nil
				}

				result, err := varDoc.GetByPath(path)
				if err != nil {
					return types.Null, nil
				}

				return result, nil
			}

			val, err := doc.Get(v)
			if err != nil {
				// Unknown variable  -- treat as a literal system variable string (e.g. $$PRUNE, $$KEEP,
				// $$DESCEND, $$REMOVE) so that operators like $redact can inspect the value.
				return v, nil
			}

			return val, nil
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
