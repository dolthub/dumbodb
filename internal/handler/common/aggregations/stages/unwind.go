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
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// unwind represents $unwind stage.
//
//	{ $unwind: <field path> }
//	{ $unwind: { path: <field path>, includeArrayIndex: <string>, preserveNullAndEmptyArrays: <bool> } }
type unwind struct {
	field                      *aggregations.Expression
	includeArrayIndex          string // empty means not set
	preserveNullAndEmptyArrays bool
	// absorbed is set when a preceding $lookup unwinds its own matches.
	absorbed bool
}

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
			switch {
			case s == "":
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnwindIndexEmpty,
					"expected a non-empty string for the includeArrayIndex  option to $unwind stage, got string", "$unwind (stage)")
			case strings.HasPrefix(s, "$"):
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnwindIndexDollar,
					"includeArrayIndex option to $unwind stage should not be prefixed with a '$': "+s, "$unwind (stage)")
			case strings.IndexByte(s, 0) >= 0:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathNulByte,
					"FieldPath field names may not contain '\\0'.", "$unwind (stage)")
			}
			if err := operators.CheckFieldPaths("$" + s); err != nil {
				return nil, err
			}
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
	// The path is a field path, never a variable.
	switch {
	case strings.HasPrefix(field, "$") && strings.ContainsRune(field, 0):
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnwindPathNulByte,
			"field path must not contain embedded null characters", "$unwind (stage)")
	case field == "$":
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrEmptyFieldPath,
			"FieldPath cannot be constructed with empty string", "$unwind (stage)")
	case strings.HasPrefix(field, "$$"):
		name, _, _ := strings.Cut(field[1:], ".")
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
			"Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "+
				"FieldPath field names may not start with '$', given '"+name+"'.", "$unwind (stage)")
	}
	if err := operators.CheckFieldPaths(field); err != nil {
		return nil, err
	}
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

func (u *unwind) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	if u.absorbed {
		return iter, nil
	}
	if u.field == nil {
		iter.Close()
		result := iterator.Values(iterator.ForSlice([]*types.Document(nil)))
		closer.Add(result)

		return result, nil
	}

	res := &unwindIterator{u: u, iter: iter, fieldPath: u.field.GetExpressionPath()}
	closer.Add(res)

	return res, nil
}

// unwindIterator emits one document per array element as it is read, so
// neither the stage's input nor a large array's expansion is held at once.
type unwindIterator struct {
	u         *unwind
	iter      types.DocumentsIterator
	fieldPath types.Path

	// doc is the input document being unwound, without its array, and
	// elements is that array.
	doc      *types.Document
	elements iterator.Interface[int, any]
}

func (it *unwindIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}

	for {
		if it.elements != nil {
			idx, v, err := it.elements.Next()
			if err == nil {
				newDoc := it.doc.DeepCopy()
				if setErr := newDoc.SetByPath(it.fieldPath, v); setErr != nil {
					return unused, nil, lazyerrors.Error(setErr)
				}
				it.u.setIndex(newDoc, int64(idx))

				return unused, newDoc, nil
			}

			it.elements.Close()
			it.elements, it.doc = nil, nil

			if !errors.Is(err, iterator.ErrIteratorDone) {
				return unused, nil, lazyerrors.Error(err)
			}
		}

		_, doc, err := it.iter.Next()
		if err != nil {
			return unused, nil, err
		}

		if out := it.start(doc); out != nil {
			return unused, out, nil
		}
	}
}

// start begins unwinding doc. It returns the document to emit when doc
// produces at most one output, or nil after queuing an array's elements or
// dropping doc.
func (it *unwindIterator) start(doc *types.Document) *types.Document {
	d, evalErr := it.u.field.Evaluate(doc)
	if evalErr != nil {
		return it.u.preserved(doc, false)
	}

	switch d := d.(type) {
	case types.NullType:
		return it.u.preserved(doc, false)

	case *types.Array:
		if d.Len() == 0 {
			return it.u.preserved(doc, true)
		}

		// Copying the array into every output would cost its size per element.
		template := doc.DeepCopy()
		if err := template.SetByPath(it.fieldPath, types.Null); err != nil {
			template = doc
		}
		it.doc, it.elements = template, d.Iterator()

		return nil

	default:
		out := doc.DeepCopy()
		it.u.setIndex(out, types.Null)

		return out
	}
}

// preserved returns doc as emitted for a missing, null or empty array value,
// or nil when such documents are dropped.
func (u *unwind) preserved(doc *types.Document, removeField bool) *types.Document {
	if !u.preserveNullAndEmptyArrays {
		return nil
	}
	out := doc.DeepCopy()
	if removeField {
		out.RemoveByPath(u.field.GetExpressionPath())
	}
	u.setIndex(out, types.Null)
	return out
}

func (u *unwind) setIndex(doc *types.Document, idx any) {
	if u.includeArrayIndex != "" {
		setNestedField(doc, u.includeArrayIndex, idx)
	}
}

func (it *unwindIterator) Close() {
	if it.elements != nil {
		it.elements.Close()
	}
	it.iter.Close()
}

var (
	_ aggregations.Stage = (*unwind)(nil)
)
