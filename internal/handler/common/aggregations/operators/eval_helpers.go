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
	"errors"
	"strings"

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/types"
)

// evalArgValue resolves an operator argument to its concrete value against the document.
// Supports:
//   - *types.Document with operator → evaluates nested operator
//   - *types.Document without operator → returns doc as-is
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

		return v, nil

	case string:
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
