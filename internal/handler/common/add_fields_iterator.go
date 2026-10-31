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

package common

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

// AddFieldsIterator returns an iterator that adds a new field to the underlying iterator.
// It will be added to the given closer.
//
// Next method returns the next document after adding the new field to the document.
//
// Close method closes the underlying iterator.
func AddFieldsIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, newField *types.Document) types.DocumentsIterator { //nolint:lll // for readability
	res := &addFieldsIterator{
		iter:     iter,
		newField: newField,
		tree:     operators.SpecHasComputedPath(newField, false) || specHasArrayValue(newField),
	}
	closer.Add(res)

	return res
}

type addFieldsIterator struct {
	iter     types.DocumentsIterator
	newField *types.Document
	// tree is set when a field is computed below the top level or is an array
	// literal, which operators.AddFieldsTree evaluates.
	tree bool
}

func specHasArrayValue(spec *types.Document) bool {
	for _, v := range spec.Values() {
		if _, ok := v.(*types.Array); ok {
			return true
		}
	}
	return false
}

func (iter *addFieldsIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}

	_, doc, err := iter.iter.Next()
	if err != nil {
		return unused, nil, lazyerrors.Error(err)
	}

	if iter.tree {
		doc, err = operators.AddFieldsTree(doc, iter.newField)
		if err = processAddFieldsError(err); err != nil {
			return unused, nil, err
		}
		return unused, doc, nil
	}

	for _, key := range iter.newField.Keys() {
		val := must.NotFail(iter.newField.Get(key))

		switch v := val.(type) {
		case *types.Document:
			if !operators.IsOperator(v) {
				break
			}

			op, err := operators.NewOperator(v)
			if err = processAddFieldsError(err); err != nil {
				return unused, nil, err
			}

			val, err = op.Process(doc)
			if errors.Is(err, operators.ErrMissingValue) {
				// A missing value removes the field, as in MongoDB.
				doc.Remove(key)
				continue
			}
			if err = processAddFieldsError(err); err != nil {
				return unused, nil, err
			}
		case string:
			// Evaluate field reference expressions like "$fieldName".
			// Non-expression strings (no "$" prefix) are used as literals.
			if strings.HasPrefix(v, "$") {
				expr, exprErr := aggregations.NewExpression(v, nil)
				if exprErr == nil {
					evaluated, evalErr := expr.Evaluate(doc)
					if evalErr == nil {
						val = evaluated
					} else {
						// A missing value removes the field, as in MongoDB.
						doc.Remove(key)
						continue
					}
				}
				// If NewExpression fails (e.g. invalid syntax), fall through and set literal.
			}
		}

		doc.Set(key, val)
	}

	return unused, doc, nil
}

func (iter *addFieldsIterator) Close() {
	iter.iter.Close()
}

// processAddFieldsError takes internal error related to operator evaluation and
// returns proper CommandError that can be returned by $addFields aggregation stage.
func processAddFieldsError(err error) error {
	return stageExpressionError(err, "$addFields")
}

// stageExpressionError is processAddFieldsError for stage, $addFields or $set.
func stageExpressionError(err error, stage string) error {
	if err == nil {
		return nil
	}

	var opErr operators.OperatorError

	if !errors.As(err, &opErr) {
		return err
	}

	switch opErr.Code() {
	case operators.ErrTooManyFields:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrAddFieldsExpressionWrongAmountOfArgs,
			"Invalid "+stage+" :: caused by :: FieldPath field names may not start with '$'."+
				" Consider using $getField or $setField.",
			stage+" (stage)",
		)
	case operators.ErrNotImplemented:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			"Invalid "+stage+" :: caused by :: "+opErr.Error(),
			stage+" (stage)",
		)
	case operators.ErrArgsInvalidLen:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrOperatorWrongLenOfArgs,
			"Invalid "+stage+" :: caused by :: "+opErr.Error(),
			stage+" (stage)",
		)
	case operators.ErrInvalidExpression:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidPipelineOperator,
			"Invalid "+stage+" :: caused by :: "+opErr.Error(),
			stage+" (stage)",
		)
	case operators.ErrInvalidNestedExpression:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidPipelineOperator,
			"Invalid "+stage+" :: caused by :: "+opErr.Error(),
			stage+" (stage)",
		)
	default:
		return lazyerrors.Error(err)
	}
}

var (
	_ types.DocumentsIterator = (*addFieldsIterator)(nil)
)

// ValidateAddFieldsExpressions returns the parse errors MongoDB raises for the
// operator expressions in a $addFields or $set specification.
func ValidateAddFieldsExpressions(stage string, spec *types.Document) error {
	for _, v := range spec.Values() {
		if err := validateSpecValue(stage, v); err != nil {
			return err
		}
	}
	return nil
}

func validateSpecValue(stage string, v any) error {
	switch v := v.(type) {
	case *types.Array:
		for i := range v.Len() {
			if err := validateSpecValue(stage, must.NotFail(v.Get(i))); err != nil {
				return err
			}
		}
	case *types.Document:
		if operators.IsOperator(v) {
			return stageExpressionError(operators.CheckExpressionShape(v), stage)
		}
		return ValidateAddFieldsExpressions(stage, v)
	}
	return nil
}
