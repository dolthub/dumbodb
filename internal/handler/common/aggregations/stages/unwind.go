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
	"errors"
	"fmt"

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/commonpath"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// unwind represents $unwind stage.
//
//	{ $unwind: <field path> }
//	{ $unwind: { path: <field path>, includeArrayIndex: <string>, preserveNullAndEmptyArrays: <bool> } }
type unwind struct {
	field                      *aggregations.Expression
	includeArrayIndex          string // empty means not set
	preserveNullAndEmptyArrays bool
}

// newUnwind creates a new $unwind stage.
func newUnwind(stage *types.Document) (aggregations.Stage, error) {
	field, err := stage.Get("$unwind")
	if err != nil {
		return nil, err
	}

	switch field := field.(type) {
	case *types.Document:
		return newUnwindFromDocument(field)
	case string:
		return newUnwindFromString(field)
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnwindWrongType,
			fmt.Sprintf(
				"expected either a string or an object as specification for $unwind stage, got %s",
				types.FormatAnyValue(field),
			),
			"$unwind (Stage)",
		)
	}
}

// newUnwindFromString creates an unwind stage from a string field path.
func newUnwindFromString(field string) (aggregations.Stage, error) {
	if field == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnwindNoPath,
			"no path specified to $unwind stage",
			"$unwind (stage)",
		)
	}

	expr, err := parseUnwindPath(field)
	if err != nil {
		return nil, err
	}

	return &unwind{field: expr}, nil
}

// newUnwindFromDocument creates an unwind stage from a document specification.
func newUnwindFromDocument(doc *types.Document) (aggregations.Stage, error) {
	pathVal, err := doc.Get("path")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnwindNoPath,
			"no path specified to $unwind stage",
			"$unwind (stage)",
		)
	}

	pathStr, ok := pathVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnwindWrongType,
			fmt.Sprintf(
				"expected either a string or an object as specification for $unwind stage, got %s",
				types.FormatAnyValue(pathVal),
			),
			"$unwind (stage)",
		)
	}

	if pathStr == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnwindNoPath,
			"no path specified to $unwind stage",
			"$unwind (stage)",
		)
	}

	expr, err := parseUnwindPath(pathStr)
	if err != nil {
		return nil, err
	}

	u := &unwind{field: expr}

	// Parse optional includeArrayIndex.
	if v, getErr := doc.Get("includeArrayIndex"); getErr == nil {
		switch s := v.(type) {
		case string:
			u.includeArrayIndex = s
		case types.NullType:
			// null is treated as not set
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("expected a string for includeArrayIndex, got %s", types.FormatAnyValue(v)),
				"$unwind (stage)",
			)
		}
	}

	// Parse optional preserveNullAndEmptyArrays.
	if v, getErr := doc.Get("preserveNullAndEmptyArrays"); getErr == nil {
		switch b := v.(type) {
		case bool:
			u.preserveNullAndEmptyArrays = b
		case types.NullType:
			// null is treated as false (default)
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("expected a boolean for preserveNullAndEmptyArrays, got %s", types.FormatAnyValue(v)),
				"$unwind (stage)",
			)
		}
	}

	// Check for any unrecognized fields.
	for _, key := range doc.Keys() {
		switch key {
		case "path", "includeArrayIndex", "preserveNullAndEmptyArrays":
			// known fields, ok
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("Unrecognized option to $unwind: %s", key),
				"$unwind (stage)",
			)
		}
	}

	return u, nil
}

// parseUnwindPath parses and validates a field path string for $unwind.
func parseUnwindPath(field string) (*aggregations.Expression, error) {
	// For $unwind to deconstruct an array from dot notation, array must be at the suffix.
	// It returns empty result if array is found at other parts of dot notation,
	// so it does not return value by index of array nor values for given key in array's document.
	expr, err := aggregations.NewExpression(field, &commonpath.FindValuesOpts{
		FindArrayIndex:     false,
		FindArrayDocuments: false,
	})
	if err != nil {
		var exprErr *aggregations.ExpressionError
		if !errors.As(err, &exprErr) {
			return nil, lazyerrors.Error(err)
		}

		switch exprErr.Code() {
		case aggregations.ErrNotExpression:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrStageUnwindNoPrefix,
				fmt.Sprintf("path option to $unwind stage should be prefixed with a '$': %v", types.FormatAnyValue(field)),
				"$unwind (stage)",
			)
		case aggregations.ErrEmptyFieldPath:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrEmptyFieldPath,
				"Expression cannot be constructed with empty string",
				"$unwind (stage)",
			)
		case aggregations.ErrEmptyVariable, aggregations.ErrInvalidExpression, aggregations.ErrUndefinedVariable:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFieldPathInvalidName,
				"Expression field names may not start with '$'. Consider using $getField or $setField",
				"$unwind (stage)",
			)
		default:
			return nil, lazyerrors.Error(err)
		}
	}

	return expr, nil
}

// Process implements Stage interface.
func (u *unwind) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var out []*types.Document

	if u.field == nil {
		result := iterator.Values(iterator.ForSlice(out))
		closer.Add(result)

		return result, nil
	}

	fieldPath := u.field.GetExpressionPath()

	for _, doc := range docs {
		d, evalErr := u.field.Evaluate(doc)

		if evalErr != nil {
			// Field does not exist in the document.
			if u.preserveNullAndEmptyArrays {
				out = append(out, doc.DeepCopy())
			}

			continue
		}

		switch d := d.(type) {
		case types.NullType:
			// Field is null.
			if u.preserveNullAndEmptyArrays {
				out = append(out, doc.DeepCopy())
			}

		case *types.Array:
			if d.Len() == 0 {
				// Empty array.
				if u.preserveNullAndEmptyArrays {
					// Emit the document with the field removed.
					newDoc := doc.DeepCopy()
					newDoc.RemoveByPath(fieldPath)
					out = append(out, newDoc)
				}

				continue
			}

			// Non-empty array: emit one doc per element.
			arrIter := d.Iterator()

			for {
				idx, v, iterErr := arrIter.Next()
				if iterErr != nil {
					arrIter.Close()

					if errors.Is(iterErr, iterator.ErrIteratorDone) {
						break
					}

					return nil, lazyerrors.Error(iterErr)
				}

				newDoc := doc.DeepCopy()

				if setErr := newDoc.SetByPath(fieldPath, v); setErr != nil {
					arrIter.Close()

					return nil, lazyerrors.Error(setErr)
				}

				if u.includeArrayIndex != "" {
					newDoc.Set(u.includeArrayIndex, int64(idx))
				}

				out = append(out, newDoc)
			}

		default:
			// Scalar value: pass through unchanged.
			out = append(out, doc.DeepCopy())
		}
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// check interfaces
var (
	_ aggregations.Stage = (*unwind)(nil)
)
