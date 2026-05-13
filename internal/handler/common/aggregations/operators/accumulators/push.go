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

package accumulators

import (
	"errors"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type pushAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
	docExpr    *types.Document // non-nil when $push arg is a document template expression
}

func newPush(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $push accumulator is a unary operator",
			"$push (accumulator)",
		)
	}

	accumulator := new(pushAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.constant = arg
			accumulator.isConst = true
		}
	case *types.Document:
		// Document expressions (e.g. { amount: "$price", date: "$ord_date" }) must be
		// evaluated per input document, not stored as a constant.
		accumulator.docExpr = arg
	default:
		accumulator.constant = arg
		accumulator.isConst = true
	}

	return accumulator, nil
}

func (p *pushAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	result := types.MakeArray(0)

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if p.docExpr != nil {
			result.Append(evalDocTemplateExpr(p.docExpr, doc))
			continue
		}

		if p.isConst {
			result.Append(p.constant)
			continue
		}

		val, evalErr := p.expression.Evaluate(doc)
		if evalErr != nil {
			// missing field: push null
			result.Append(types.Null)
			continue
		}

		result.Append(val)
	}

	return result, nil
}

// evalDocTemplateExpr evaluates a document template expression against an input document.
// Each value in the template is evaluated:
//   - Operator documents (e.g. {$add: [...]}) are processed via the operator.
//   - String field references (e.g. "$price") are resolved against doc.
//   - All other values are used as literals.
//
// Missing field references result in a null entry (MongoDB behavior).
func evalDocTemplateExpr(tmpl, doc *types.Document) *types.Document {
	if operators.IsOperator(tmpl) {
		op, err := operators.NewOperator(tmpl)
		if err != nil {
			return types.MakeDocument(0)
		}

		val, err := op.Process(doc)
		if err != nil {
			return types.MakeDocument(0)
		}

		if d, ok := val.(*types.Document); ok {
			return d
		}

		return types.MakeDocument(0)
	}

	result := types.MakeDocument(0)

	for _, key := range tmpl.Keys() {
		val := must.NotFail(tmpl.Get(key))

		switch v := val.(type) {
		case string:
			if strings.HasPrefix(v, "$") {
				expr, err := aggregations.NewExpression(v, nil)
				if err == nil {
					evaluated, evalErr := expr.Evaluate(doc)
					if evalErr == nil {
						result.Set(key, evaluated)
					} else {
						result.Set(key, types.Null)
					}

					continue
				}
			}

			result.Set(key, val)
		case *types.Document:
			result.Set(key, evalDocTemplateExpr(v, doc))
		default:
			result.Set(key, val)
		}
	}

	return result
}

var (
	_ Accumulator = (*pushAccumulator)(nil)
)
