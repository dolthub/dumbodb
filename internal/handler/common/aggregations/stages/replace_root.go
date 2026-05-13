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
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
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

	return &replaceRoot{newRoot: newRootVal}, nil
}

func (r *replaceRoot) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	out := make([]*types.Document, 0, len(docs))

	for _, doc := range docs {
		replacement, evalErr := evaluateReplaceExpression(r.newRoot, doc)
		if evalErr != nil {
			return nil, evalErr
		}

		out = append(out, replacement)
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// evaluateReplaceExpression evaluates a newRoot expression against a document.
// It supports field path expressions (e.g. "$subdoc") and literal documents.
func evaluateReplaceExpression(expr any, doc *types.Document) (*types.Document, error) {
	switch e := expr.(type) {
	case string:
		// Field path expression like "$subdoc".
		fieldExpr, err := aggregations.NewExpression(e, nil)
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("invalid expression for $replaceRoot: %s", e),
				"$replaceRoot (stage)",
			)
		}

		val, err := fieldExpr.Evaluate(doc)
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrOperationFailed,
				"'newRoot' expression for $replaceRoot must evaluate to an object",
				"$replaceRoot (stage)",
			)
		}

		result, ok := val.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrOperationFailed,
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
					handlererrors.ErrOperationFailed,
					fmt.Sprintf(
						"'newRoot' expression for $replaceRoot must evaluate to an object, got %s",
						types.FormatAnyValue(val),
					),
					"$replaceRoot (stage)",
				)
			}

			return result, nil
		}

		// Literal document template  -- evaluate field path expressions in values.
		return evaluateDocumentExpression(e, doc)

	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf(
				"'newRoot' expression for $replaceRoot must evaluate to an object, got %s",
				types.FormatAnyValue(expr),
			),
			"$replaceRoot (stage)",
		)
	}
}

// evaluateDocumentExpression processes a document where values may be field path expressions.
// For each value that is a string starting with "$", it evaluates the expression against doc.
// Other values are copied as literals.
func evaluateDocumentExpression(templateDoc *types.Document, doc *types.Document) (*types.Document, error) {
	result, err := types.NewDocument()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	iter := templateDoc.Iterator()
	defer iter.Close()

	for {
		k, v, iterErr := iter.Next()
		if iterErr != nil {
			break
		}

		var outVal any

		switch val := v.(type) {
		case string:
			if len(val) > 0 && val[0] == '$' {
				fieldExpr, exprErr := aggregations.NewExpression(val, nil)
				if exprErr != nil {
					outVal = val // treat as literal if not a valid expression
				} else {
					evaluated, evalErr := fieldExpr.Evaluate(doc)
					if evalErr != nil {
						outVal = types.Null
					} else {
						outVal = evaluated
					}
				}
			} else {
				outVal = val
			}

		case *types.Document:
			// Operator expression (e.g. {$mergeObjects: [...]})  -- evaluate it.
			if operators.IsOperator(val) {
				op, opErr := operators.NewOperator(val)
				if opErr != nil {
					return nil, lazyerrors.Error(opErr)
				}

				evaluated, opErr := op.Process(doc)
				if opErr != nil {
					return nil, lazyerrors.Error(opErr)
				}

				outVal = evaluated
			} else {
				// Recursively evaluate nested literal documents.
				nested, evalErr := evaluateDocumentExpression(val, doc)
				if evalErr != nil {
					return nil, evalErr
				}

				outVal = nested
			}

		default:
			outVal = v
		}

		result.Set(k, outVal)
	}

	return result, nil
}

var (
	_ aggregations.Stage = (*replaceRoot)(nil)
)
