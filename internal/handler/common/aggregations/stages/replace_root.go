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

package stages

import (
	"context"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// replaceRoot represents $replaceRoot stage.
//
//	{ $replaceRoot: { newRoot: <replacement document expression> } }
type replaceRoot struct {
	newRoot any // expression or literal document
}

func newReplaceRoot(stage *types.Document) (aggregations.Stage, error) {
	spec, err := stage.Get("$replaceRoot")
	if err != nil {
		return nil, err
	}

	specDoc, ok := spec.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$replaceRoot specification must be an object, got %s", types.FormatAnyValue(spec)),
			"$replaceRoot (stage)",
		)
	}

	newRootVal, err := specDoc.Get("newRoot")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$replaceRoot requires a 'newRoot' option",
			"$replaceRoot (stage)",
		)
	}

	if err := operators.CheckConstantSubexpressions(newRootVal); err != nil {
		return nil, err
	}

	return &replaceRoot{newRoot: newRootVal}, nil
}

// newReplaceWith creates a new $replaceWith stage (shorthand for $replaceRoot).
//
//	{ $replaceWith: <replacement document expression> }
func newReplaceWith(stage *types.Document) (aggregations.Stage, error) {
	newRootVal, err := stage.Get("$replaceWith")
	if err != nil {
		return nil, err
	}

	if err := operators.CheckConstantSubexpressions(newRootVal); err != nil {
		return nil, err
	}

	return &replaceRoot{newRoot: newRootVal}, nil
}

func (r *replaceRoot) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return newMapIterator(iter, closer, func(doc *types.Document) (*types.Document, error) {
		return evaluateReplaceExpression(r.newRoot, doc)
	}), nil
}

// evaluateReplaceExpression evaluates a newRoot expression against a document.
// It supports field path expressions (e.g. "$subdoc") and literal documents.
func evaluateReplaceExpression(expr any, doc *types.Document) (*types.Document, error) {
	switch e := expr.(type) {
	case string:
		val, err := operators.EvalArgValue(e, doc)
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("invalid expression for $replaceRoot: %s", e),
				"$replaceRoot (stage)",
			)
		}

		result, ok := val.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrReplaceRootNotObject,
				fmt.Sprintf(
					"'newRoot' expression for $replaceRoot must evaluate to an object, got %s",
					types.FormatAnyValue(val),
				),
				"$replaceRoot (stage)",
			)
		}

		return result.DeepCopy(), nil

	case *types.Document:
		// Operator expression like {$mergeObjects: [...]}  -- evaluate as operator.
		if operators.IsOperator(e) {
			op, opErr := operators.NewOperator(e)
			if opErr != nil {
				// MongoDB reports a field-operator rejection under the operator's
				// own code and bare message here, with no stage wrapper.
				if code, ok := operators.FieldOpErrorCode(opErr); ok {
					return nil, handlererrors.NewCommandErrorMsgWithArgument(
						code,
						opErr.Error(),
						"$replaceRoot (stage)",
					)
				}

				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrOperationFailed,
					fmt.Sprintf("'newRoot' expression for $replaceRoot failed: %s", opErr.Error()),
					"$replaceRoot (stage)",
				)
			}

			val, opErr := op.Process(doc)
			if opErr != nil {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrOperationFailed,
					fmt.Sprintf("'newRoot' expression for $replaceRoot failed: %s", opErr.Error()),
					"$replaceRoot (stage)",
				)
			}

			result, ok := val.(*types.Document)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrReplaceRootNotObject,
					fmt.Sprintf(
						"'newRoot' expression for $replaceRoot must evaluate to an object, got %s",
						types.FormatAnyValue(val),
					),
					"$replaceRoot (stage)",
				)
			}

			return result, nil
		}

		// A document literal, evaluated as MongoDB's ExpressionObject.
		v, _, err := operators.EvalOperand(e, doc)
		if err != nil {
			return nil, err
		}
		return v.(*types.Document), nil

	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrReplaceRootNotObject,
			fmt.Sprintf(
				"'newRoot' expression for $replaceRoot must evaluate to an object, got %s",
				types.FormatAnyValue(expr),
			),
			"$replaceRoot (stage)",
		)
	}
}

var (
	_ aggregations.Stage = (*replaceRoot)(nil)
)
