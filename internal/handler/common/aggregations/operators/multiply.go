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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// multiply represents `$multiply` expression operator.
type multiply struct {
	expressions []*aggregations.Expression
	operators   []*types.Document
	numbers     []any
	rawArgs     []string // variable references ($$var) or other raw strings for evalArgValue
}

// newMultiply creates a new $multiply operator.
// It accepts an array of numeric arguments (literals or field paths).
func newMultiply(args ...any) (Operator, error) {
	op := new(multiply)

	for _, arg := range args {
		switch arg := arg.(type) {
		case *types.Document:
			if IsOperator(arg) {
				op.operators = append(op.operators, arg)
			}
		case string:
			ex, err := aggregations.NewExpression(arg, nil)

			var exErr *aggregations.ExpressionError
			if errors.As(err, &exErr) {
				if exErr.Code() == aggregations.ErrUndefinedVariable {
					// Variable reference ($$var) — evaluate at process time via evalArgValue.
					op.rawArgs = append(op.rawArgs, arg)
				}

				// ErrNotExpression and other codes: skip this arg.
				continue
			}

			if err != nil {
				return nil, err
			}

			op.expressions = append(op.expressions, ex)
		case float64, int32, int64, types.Decimal128:
			op.numbers = append(op.numbers, arg)
		}
	}

	return op, nil
}

// Process implements Operator interface.
// It evaluates all expressions, computes nested operators, and multiplies
// all resulting numeric values together.
func (m *multiply) Process(doc *types.Document) (any, error) {
	var values []any

	for _, expression := range m.expressions {
		value, err := expression.Evaluate(doc)
		if err != nil {
			continue
		}

		switch v := value.(type) {
		case *types.Array:
			iter := v.Iterator()
			defer iter.Close()

			for {
				_, elem, err := iter.Next()
				if errors.Is(err, iterator.ErrIteratorDone) {
					break
				}

				if err != nil {
					return nil, lazyerrors.Error(err)
				}

				values = append(values, elem)
			}
		default:
			values = append(values, value)
		}
	}

	for _, operatorExpr := range m.operators {
		op, err := NewOperator(operatorExpr)
		if err != nil {
			return nil, err
		}

		v, err := op.Process(doc)
		if err != nil {
			return nil, err
		}

		values = append(values, v)
	}

	for _, rawArg := range m.rawArgs {
		v, err := evalArgValue(rawArg, doc)
		if err != nil {
			continue
		}

		values = append(values, v)
	}

	for _, n := range m.numbers {
		values = append(values, n)
	}

	return aggregations.MultiplyNumbers(values...), nil
}

// check interfaces
var (
	_ Operator = (*multiply)(nil)
)
