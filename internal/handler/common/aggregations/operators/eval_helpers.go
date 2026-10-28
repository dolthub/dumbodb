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
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func EvalArgValue(arg any, doc *types.Document) (any, error) {
	return evalArgValue(arg, doc)
}

// EvalOperand evaluates any aggregation expression, reporting whether it
// produced MongoDB's missing value (an absent field or $$REMOVE). As in
// MongoDB, a document literal omits fields that are missing and an array
// literal holds null in their place.
func EvalOperand(expr any, doc *types.Document) (value any, missing bool, err error) {
	switch v := expr.(type) {
	case *types.Document:
		if IsOperator(v) {
			op, err := cachedOperator(v)
			if err != nil {
				return nil, false, err
			}
			value, err = op.Process(doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				return nil, true, nil
			}
			return value, false, err
		}
		value, err = evalDocumentExpressions(v, doc)
		return value, false, err

	case *types.Array:
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			elem, elemMissing, err := EvalOperand(must.NotFail(v.Get(i)), doc)
			if err != nil {
				return nil, false, err
			}
			if elemMissing {
				elem = types.Null
			}
			out.Append(elem)
		}
		return out, false, nil

	case string:
		if v == "$$REMOVE" {
			return nil, true, nil
		}
		value, missing, err = evalArgValueOrMissing(v, doc)
		if errors.Is(err, aggregations.ErrMissingValue) {
			return nil, true, nil
		}
		return value, missing, err

	default:
		return v, false, nil
	}
}

// evalBranch evaluates an expression an operator returns as is, such as a
// $cond branch, so that a missing value stays missing (ErrMissingValue).
func evalBranch(arg any, doc *types.Document) (any, error) {
	v, missing, err := EvalOperand(arg, doc)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, aggregations.ErrMissingValue
	}
	return v, nil
}

// evalArgValue evaluates an operand. A missing field, or an operator that
// produces the missing value, reads as null, as MongoDB's operators treat
// missing operands as nullish; $$REMOVE alone is reported as ErrMissingValue.
func evalArgValue(arg any, doc *types.Document) (any, error) {
	switch v := arg.(type) {
	case *types.Document:
		if IsOperator(v) {
			op, err := cachedOperator(v)
			if err != nil {
				return nil, err
			}

			res, err := op.Process(doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				return types.Null, nil
			}
			return res, err
		}

		return evalDocumentExpressions(v, doc)

	case *types.Array:
		// An array literal is an expression: each element is evaluated, and a
		// missing element becomes null.
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			elem, err := evalArgValue(must.NotFail(v.Get(i)), doc)
			if errors.Is(err, aggregations.ErrMissingValue) {
				elem, err = types.Null, nil
			}
			if err != nil {
				return nil, err
			}
			out.Append(elem)
		}
		return out, nil

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
			case "NOW":
				base = time.Now().UTC()
			case "REMOVE":
				// Missing rather than null, so a $cond selecting $$REMOVE drops
				// the field instead of writing an explicit null.
				return nil, aggregations.ErrMissingValue
			case "PRUNE", "KEEP", "DESCEND":
				// $redact reads these as literal strings to decide its action.
				base = v
			default:
				val, err := doc.Get("$$" + varName)
				if err != nil {
					return nil, handlererrors.NewCommandErrorMsg(
						handlererrors.ErrGroupUndefinedVariable,
						"Use of undefined variable: "+varName,
					)
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
			expr, err := fieldExpression(v)
			if err != nil {
				var exErr *aggregations.ExpressionError
				if errors.As(err, &exErr) && exErr.Code() == aggregations.ErrNotExpression {
					return v, nil
				}

				return nil, err
			}

			val, err := expr.Evaluate(doc)
			if err != nil {
				return types.Null, nil
			}

			return val, nil
		}

		return v, nil

	default:
		return v, nil
	}
}

// evalDocumentExpressions evaluates a document literal as MongoDB's
// ExpressionObject does: a field whose value is missing is omitted.
func evalDocumentExpressions(expr *types.Document, doc *types.Document) (*types.Document, error) {
	result := types.MakeDocument(expr.Len())

	for _, k := range expr.Keys() {
		evaluated, missing, err := EvalOperand(must.NotFail(expr.Get(k)), doc)
		if err != nil {
			return nil, err
		}
		if !missing {
			result.Set(k, evaluated)
		}
	}

	return result, nil
}

// isNullish reports whether v is null or undefined, which expressions treat
// alike.
func isNullish(v any) bool {
	return v == types.Null || v == types.Undefined
}

// isFalsy returns true when v is MongoDB-falsy for $cond purposes:
// false (bool), null, or missing.
func isFalsy(v any) bool {
	switch val := v.(type) {
	case bool:
		return !val
	case types.NullType, types.UndefinedType:
		return true
	default:
		_ = val
		return false
	}
}
