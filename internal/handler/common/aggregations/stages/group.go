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
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators/accumulators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// group represents $group stage.
//
//	{ $group: {
//		_id: <groupExpression>,
//		<groupBy[0].outputField>: {accumulator0: expression0},
//		...
//		<groupBy[N].outputField>: {accumulatorN: expressionN},
//	}}
//
// $group uses group expression to group documents that have the same evaluated expression.
// The evaluated expression becomes the _id for that group of documents.
// For each group of documents, accumulators are applied.
type group struct {
	groupExpression any
	// groupExprCompiled caches a parsed aggregations.Expression when
	// groupExpression is a "$field" string. Parsing it once up front avoids
	// per-document path parsing in the hot loop.
	groupExprCompiled *aggregations.Expression
	groupBy           []groupBy
}

// groupBy represents accumulation to apply on the group.
type groupBy struct {
	accumulator accumulators.Accumulator
	outputField string
}

// newGroup creates a new $group stage.
func newGroup(stage *types.Document) (aggregations.Stage, error) {
	fields, err := common.GetRequiredParam[*types.Document](stage, "$group")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupInvalidFields,
			"a group's fields must be specified in an object",
			"$group (stage)",
		)
	}

	var groupKey any
	var groups []groupBy

	iter := fields.Iterator()

	defer iter.Close()

	for {
		field, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if field == "_id" {
			if err = validateGroupKey(v); err != nil {
				return nil, err
			}

			groupKey = v
			continue
		}

		accumulator, err := accumulators.NewAccumulator("$group", field, v)
		if err != nil {
			return nil, processGroupStageError(err)
		}

		groups = append(groups, groupBy{
			outputField: field,
			accumulator: accumulator,
		})
	}

	if groupKey == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupMissingID,
			"a group specification must include an _id",
			"$group (stage)",
		)
	}

	g := &group{
		groupExpression: groupKey,
		groupBy:         groups,
	}

	if s, ok := groupKey.(string); ok {
		// Pre-compile once; ErrNotExpression means the literal is used as-is.
		if expr, err := aggregations.NewExpression(s, nil); err == nil {
			g.groupExprCompiled = expr
		}
	}

	return g, nil
}

// Process implements Stage interface.
func (g *group) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	groupedDocuments, err := g.groupDocuments(iter)
	if err != nil {
		return nil, err
	}

	var res []*types.Document

	for _, groupedDocument := range groupedDocuments {
		doc := must.NotFail(types.NewDocument("_id", groupedDocument.groupID))

		for _, accumulation := range g.groupBy {
			groupIter := iterator.Values(iterator.ForSlice(groupedDocument.documents))
			out, err := accumulation.accumulator.Accumulate(groupIter)
			if err != nil {
				// existing accumulators do not return error
				return nil, processGroupStageError(err)
			}

			if doc.Has(accumulation.outputField) {
				// document has duplicate key
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrStageIndexedStringVectorDuplicate,
					fmt.Sprintf("duplicate field: %s", accumulation.outputField),
					"$group (stage)",
				)
			}

			doc.Set(accumulation.outputField, out)
		}

		res = append(res, doc)
	}

	iter = iterator.Values(iterator.ForSlice(res))
	closer.Add(iter)

	return iter, nil
}

// validateGroupKey returns error on invalid group key.
// If group key is a document, it recursively validates operator and expression.
func validateGroupKey(groupKey any) error {
	doc, ok := groupKey.(*types.Document)
	if !ok {
		return nil
	}

	if operators.IsOperator(doc) {
		op, err := operators.NewOperator(doc)
		if err != nil {
			return processGroupStageError(err)
		}

		_, err = op.Process(nil)
		if err != nil {
			return processGroupStageError(err)
		}
	}

	iter := doc.Iterator()
	defer iter.Close()

	fields := make(map[string]struct{}, doc.Len())

	for {
		k, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return lazyerrors.Error(err)
		}

		if _, ok := fields[k]; ok {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGroupDuplicateFieldName,
				fmt.Sprintf("duplicate field name specified in object literal: %s", types.FormatAnyValue(doc)),
				"$group (stage)",
			)
		}
		fields[k] = struct{}{}

		switch v := v.(type) {
		case *types.Document:
			return validateGroupKey(v)
		case string:
			_, err := aggregations.NewExpression(v, nil)
			var exprErr *aggregations.ExpressionError

			if errors.As(err, &exprErr) && exprErr.Code() == aggregations.ErrNotExpression {
				err = nil
			}

			if err != nil {
				return processGroupStageError(err)
			}
		}
	}

	return nil
}

// groupDocuments groups documents into groups using group key. If group key contains expressions
// or operators, they are evaluated before using it as the group key of documents.
func (g *group) groupDocuments(iter types.DocumentsIterator) ([]groupedDocuments, error) {
	defer iter.Close()

	var m groupMap

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch groupKey := g.groupExpression.(type) {
		case *types.Document:
			val, err := evaluateDocument(groupKey, doc, false)
			if err != nil {
				// operator and expression errors are validated in newGroup
				return nil, lazyerrors.Error(err)
			}

			m.addOrAppend(val, doc)
		case *types.Array, float64, types.Binary, types.ObjectID, bool, time.Time, types.NullType,
			types.Regex, int32, types.Timestamp, int64:
			m.addOrAppend(groupKey, doc)
		case string:
			// g.groupExprCompiled is set iff groupKey is a valid "$path"
			// expression; a plain literal leaves it nil and is added as-is.
			if g.groupExprCompiled == nil {
				m.addOrAppend(groupKey, doc)
				continue
			}

			val, err := g.groupExprCompiled.Evaluate(doc)
			if err != nil {
				// $group treats non-existent fields as nulls
				val = types.Null
			}

			m.addOrAppend(val, doc)
		default:
			panic(fmt.Sprintf("unexpected type %[1]T (%#[1]v)", groupKey))
		}
	}

	return m.docs, nil
}

// evaluateDocument recursively evaluates document's field expressions and operators.
func evaluateDocument(expr, doc *types.Document, nestedField bool) (any, error) {
	if operators.IsOperator(expr) {
		op, err := operators.NewOperator(expr)
		if err != nil {
			// operator error was validated in newGroup
			return nil, processGroupStageError(err)
		}

		v, err := op.Process(doc)
		if err != nil {
			// operator and expression errors are validated in newGroup
			return nil, processGroupStageError(err)
		}

		return v, nil
	}

	iter := expr.Iterator()
	defer iter.Close()

	evaluatedDocument := new(types.Document)

	for {
		k, exprVal, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch exprVal := exprVal.(type) {
		case *types.Document:
			v, err := evaluateDocument(exprVal, doc, true)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			evaluatedDocument.Set(k, v)
		case string:
			expression, err := aggregations.NewExpression(exprVal, nil)

			var exprErr *aggregations.ExpressionError
			if errors.As(err, &exprErr) && exprErr.Code() == aggregations.ErrNotExpression {
				evaluatedDocument.Set(k, exprVal)
				continue
			}

			if err != nil {
				// expression error was validated in newGroup.
				return nil, lazyerrors.Error(err)
			}

			v, err := expression.Evaluate(doc)
			if err != nil {
				if expr.Len() == 1 && !nestedField {
					// non-existent path is set to null if expression contains single field and not a nested document
					evaluatedDocument.Set(k, types.Null)
				}

				continue
			}

			evaluatedDocument.Set(k, v)
		default:
			evaluatedDocument.Set(k, exprVal)
		}
	}

	return evaluatedDocument, nil
}

// groupedDocuments contains group key and the documents for that group.
type groupedDocuments struct {
	groupID   any
	documents []*types.Document
}

// groupMap holds groups of documents.
//
// Group keys can be any BSON type (including arrays and binaries) and
// numeric types are grouped numerically regardless of int/int64/float — so
// the general path falls back to a linear scan with types.CompareForAggregation.
// For hashable, comparably-typed keys (strings, bools, int32/int64, ObjectID)
// a fast path map indexes into docs, cutting O(n*k) group lookups to O(n).
type groupMap struct {
	docs []groupedDocuments
	// fast indexes groupedDocuments by key for keys that cannot collide
	// with numerically-equal keys of another Go type. Numeric keys are
	// intentionally excluded because CompareForAggregation treats
	// int32(1)/int64(1)/float64(1.0) as equal.
	fast map[any]int
}

// addOrAppend adds a groupID documents pair if the groupID does not exist,
// if the groupID exists it appends the documents to the slice.
func (m *groupMap) addOrAppend(groupKey any, docs ...*types.Document) {
	if hashable, key := hashableGroupKey(groupKey); hashable {
		if m.fast == nil {
			m.fast = make(map[any]int)
		}

		if i, ok := m.fast[key]; ok {
			m.docs[i].documents = append(m.docs[i].documents, docs...)
			return
		}

		// No entry yet — but a numeric-equal entry might already exist in
		// m.docs from a prior add with a differently-typed numeric key.
		// Scan once to stay consistent with CompareForAggregation semantics.
		for i, g := range m.docs {
			if types.CompareForAggregation(groupKey, g.groupID) == types.Equal {
				m.docs[i].documents = append(m.docs[i].documents, docs...)
				m.fast[key] = i
				return
			}
		}

		m.fast[key] = len(m.docs)
		m.docs = append(m.docs, groupedDocuments{
			groupID:   groupKey,
			documents: docs,
		})
		return
	}

	for i, g := range m.docs {
		// groupID is a distinct key and can be any BSON type including array and Binary,
		// so we cannot use structure like map.
		// Compare is used to check if groupID exists in groupMap, because
		// numbers are grouped for the same value regardless of their number type.
		if types.CompareForAggregation(groupKey, g.groupID) == types.Equal {
			m.docs[i].documents = append(m.docs[i].documents, docs...)
			return
		}
	}

	m.docs = append(m.docs, groupedDocuments{
		groupID:   groupKey,
		documents: docs,
	})
}

// hashableGroupKey returns true and a map-friendly key when groupKey is a
// Go-comparable type. Numeric types are normalized into a float64 bucket
// because CompareForAggregation treats int32(1)/int64(1)/float64(1.0) as
// equal; a mismatched numeric hit is disambiguated in addOrAppend via a
// CompareForAggregation scan on first insert.
func hashableGroupKey(groupKey any) (bool, any) {
	switch k := groupKey.(type) {
	case string:
		return true, k
	case bool:
		return true, k
	case types.ObjectID:
		return true, k
	case types.NullType:
		return true, k
	case int32:
		return true, float64(k)
	case int64:
		return true, float64(k)
	case float64:
		return true, k
	case time.Time:
		return true, k.UnixNano()
	}
	return false, nil
}

// processGroupStageError takes internal error related to operator evaluation and
// expression evaluation and returns CommandError that can be returned by $group
// aggregation stage.
func processGroupStageError(err error) error {
	var opErr operators.OperatorError
	var exErr *aggregations.ExpressionError

	switch {
	case errors.As(err, &opErr):
		switch opErr.Code() {
		case operators.ErrTooManyFields:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrExpressionWrongLenOfFields,
				opErr.Error(),
				"$group (stage)",
			)
		case operators.ErrNotImplemented:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"Invalid $group :: caused by :: "+opErr.Error(),
				"$group (stage)",
			)
		case operators.ErrArgsInvalidLen:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrOperatorWrongLenOfArgs,
				opErr.Error(),
				"$group (stage)",
			)
		case operators.ErrInvalidExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidPipelineOperator,
				opErr.Error(),
				"$group (stage)",
			)
		case operators.ErrInvalidNestedExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidPipelineOperator,
				opErr.Error(),
				"$group (stage)",
			)
		}

	case errors.As(err, &exErr):
		switch exErr.Code() {
		case aggregations.ErrNotExpression:
			// handled by upstream and this should not be reachable for existing expression implementation
			fallthrough
		case aggregations.ErrInvalidExpression:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("'%s' starts with an invalid character for a user variable name", exErr.Name()),
				"$group (stage)",
			)
		case aggregations.ErrEmptyFieldPath:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGroupInvalidFieldPath,
				"'$' by itself is not a valid FieldPath",
				"$group (stage)",
			)
		case aggregations.ErrUndefinedVariable:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"Aggregation expression variables are not implemented yet",
				"$group (stage)",
			)
		case aggregations.ErrEmptyVariable:
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"empty variable names are not allowed",
				"$group (stage)",
			)
		}
	}

	return err
}

// check interfaces
var (
	_ aggregations.Stage = (*group)(nil)
)
