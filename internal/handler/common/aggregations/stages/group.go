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
	"strconv"
	"strings"
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

type groupBy struct {
	accumulator accumulators.Accumulator
	outputField string
}

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

func (g *group) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	defer iter.Close()

	// Fold each document into its group's accumulators as it streams; documents
	// are never retained, so memory scales with the number of groups (and any
	// inherently-retaining accumulators like $push), not the input size.
	gm := newGroupMap(g.groupBy)

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		groupKey, err := g.evalGroupKey(doc)
		if err != nil {
			return nil, err
		}

		if err := gm.accumulate(groupKey, doc); err != nil {
			// existing accumulators rarely return error
			return nil, processGroupStageError(err)
		}
	}

	var res []*types.Document

	for i := range gm.docs {
		grp := &gm.docs[i]

		doc := must.NotFail(types.NewDocument("_id", grp.groupID))

		for j, acc := range grp.accs {
			out, err := acc.Result()
			if err != nil {
				return nil, processGroupStageError(err)
			}

			field := g.groupBy[j].outputField

			if doc.Has(field) {
				// document has duplicate key
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrStageIndexedStringVectorDuplicate,
					fmt.Sprintf("duplicate field: %s", field),
					"$group (stage)",
				)
			}

			doc.Set(field, out)
		}

		res = append(res, doc)
	}

	resIter := iterator.Values(iterator.ForSlice(res))
	closer.Add(resIter)

	return resIter, nil
}

// evalGroupKey computes the _id group key for one document. It mirrors the type
// handling of the group expression: documents are evaluated recursively,
// "$field" strings resolve via the precompiled expression (missing -> null),
// and other literal BSON types are used as-is.
func (g *group) evalGroupKey(doc *types.Document) (any, error) {
	switch groupKey := g.groupExpression.(type) {
	case *types.Document:
		return evaluateDocument(groupKey, doc, false)
	case *types.Array, float64, types.Binary, types.ObjectID, bool, time.Time, types.NullType,
		types.Regex, int32, types.Timestamp, int64:
		return groupKey, nil
	case string:
		// g.groupExprCompiled is set iff groupKey is a valid "$path"
		// expression; a plain literal leaves it nil and is used as-is.
		if g.groupExprCompiled == nil {
			return groupKey, nil
		}

		val, err := g.groupExprCompiled.Evaluate(doc)
		if err != nil {
			// $group treats non-existent fields as nulls
			return types.Null, nil
		}

		return val, nil
	default:
		panic(fmt.Sprintf("unexpected type %[1]T (%#[1]v)", groupKey))
	}
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

type groupedDocuments struct {
	groupID any
	// accs holds one live accumulation per groupBy spec, folded as documents
	// stream in. Documents themselves are not retained.
	accs []accumulators.Accumulation
}

// groupMap holds the running accumulators for each group.
//
// Group keys can be any BSON type (including arrays and binaries) and
// numeric types are grouped numerically regardless of int/int64/float  -- so
// the general path falls back to a linear scan with types.CompareForAggregation.
// For hashable, comparably-typed keys (strings, bools, int32/int64, ObjectID)
// a fast path map indexes into docs, cutting O(n*k) group lookups to O(n).
type groupMap struct {
	specs []groupBy
	docs  []groupedDocuments
	// fast indexes docs by a canonical string key so groups (including
	// array and document keys) resolve in O(1) instead of a linear
	// CompareForAggregation scan. Numeric keys hash lossily and are
	// reconciled by a one-time backstop scan; see index.
	fast map[string]int
}

func newGroupMap(specs []groupBy) *groupMap {
	return &groupMap{specs: specs}
}

// accumulate routes doc into its group (creating the group and its fresh
// accumulators on first sight) and folds doc through each accumulator.
func (m *groupMap) accumulate(groupKey any, doc *types.Document) error {
	idx := m.index(groupKey)

	for _, acc := range m.docs[idx].accs {
		if err := acc.Accumulate(doc); err != nil {
			return err
		}
	}

	return nil
}

// index returns the slot for groupKey, creating a new group with fresh
// accumulators when none exists yet.
func (m *groupMap) index(groupKey any) int {
	key, numericFree := canonicalGroupKey(groupKey)

	if m.fast == nil {
		m.fast = make(map[string]int)
	}

	if i, ok := m.fast[key]; ok {
		return i
	}

	// A numeric key hashes lossily: differently-typed but equal numbers
	// (int32(1)/int64(1)/float64(1.0)/Decimal128(1)) must land in one group,
	// and float64 normalization can collide distinct large integers. Reconcile
	// against existing groups via CompareForAggregation before creating a new
	// one. Numeric-free keys are exact, so they skip the scan entirely.
	if !numericFree {
		for i := range m.docs {
			if types.CompareForAggregation(groupKey, m.docs[i].groupID) == types.Equal {
				m.fast[key] = i
				return i
			}
		}
	}

	i := m.newGroup(groupKey)
	m.fast[key] = i

	return i
}

// newGroup appends a group for groupKey with a fresh accumulation per spec and
// returns its index.
func (m *groupMap) newGroup(groupKey any) int {
	accs := make([]accumulators.Accumulation, len(m.specs))
	for i := range m.specs {
		accs[i] = m.specs[i].accumulator.New()
	}

	m.docs = append(m.docs, groupedDocuments{groupID: groupKey, accs: accs})

	return len(m.docs) - 1
}

// canonicalGroupKey builds a string that is identical for two group keys
// exactly when they should land in the same group, so lookups are O(1). The
// returned bool is true when the key contains no numeric value: numeric-free
// keys are exact, while numeric keys hash lossily and need the backstop scan
// in index. Arrays and documents are encoded recursively, which is the case
// the old scalar-only fast path missed.
func canonicalGroupKey(groupKey any) (string, bool) {
	var b strings.Builder
	numericFree := appendGroupKey(&b, groupKey)
	return b.String(), numericFree
}

// appendGroupKey writes v's canonical encoding and reports whether v (and
// everything nested in it) is numeric-free. Length-prefixing keeps the
// encoding injective so distinct values never share a key.
func appendGroupKey(b *strings.Builder, v any) bool {
	switch v := v.(type) {
	case *types.Document:
		b.WriteByte('D')
		numericFree := true
		for _, k := range v.Keys() {
			val, _ := v.Get(k)
			appendLenString(b, k)
			if !appendGroupKey(b, val) {
				numericFree = false
			}
		}
		b.WriteByte(';')
		return numericFree
	case *types.Array:
		b.WriteByte('A')
		numericFree := true
		for i := 0; i < v.Len(); i++ {
			val, _ := v.Get(i)
			if !appendGroupKey(b, val) {
				numericFree = false
			}
		}
		b.WriteByte(';')
		return numericFree
	case string:
		b.WriteByte('s')
		appendLenString(b, v)
		return true
	case bool:
		if v {
			b.WriteByte('T')
		} else {
			b.WriteByte('F')
		}
		return true
	case types.NullType:
		b.WriteByte('z')
		return true
	case types.ObjectID:
		b.WriteByte('o')
		b.Write(v[:])
		return true
	case time.Time:
		b.WriteByte('t')
		b.WriteString(strconv.FormatInt(v.UnixNano(), 10))
		b.WriteByte(';')
		return true
	case types.Timestamp:
		b.WriteByte('u')
		b.WriteString(strconv.FormatUint(uint64(v), 10))
		b.WriteByte(';')
		return true
	case types.Regex:
		b.WriteByte('r')
		appendLenString(b, v.Pattern)
		appendLenString(b, v.Options)
		return true
	case types.Binary:
		b.WriteByte('x')
		b.WriteByte(byte(v.Subtype))
		appendLenString(b, string(v.B))
		return true
	case types.MinKeyType:
		b.WriteByte('<')
		return true
	case types.MaxKeyType:
		b.WriteByte('>')
		return true
	case int32:
		appendNumericKey(b, float64(v))
		return false
	case int64:
		appendNumericKey(b, float64(v))
		return false
	case float64:
		appendNumericKey(b, v)
		return false
	case types.Decimal128:
		// Decimal128 is not folded into the float64 bucket; equal values with
		// a different representation (or an equal int/float) are reconciled by
		// the backstop scan. numericFree is false so that scan runs.
		b.WriteByte('n')
		b.WriteString(strconv.FormatUint(v.H, 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(v.L, 10))
		b.WriteByte(';')
		return false
	default:
		// Unknown type: emit a per-type marker and force the backstop scan.
		fmt.Fprintf(b, "?%T;", v)
		return false
	}
}

// appendNumericKey writes a numeric marker plus the exact float64 bit pattern
// so int32(1)/int64(1)/float64(1.0) share one bucket.
func appendNumericKey(b *strings.Builder, f float64) {
	b.WriteByte('#')
	b.WriteString(strconv.FormatFloat(f, 'b', -1, 64))
	b.WriteByte(';')
}

func appendLenString(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
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

var (
	_ aggregations.Stage = (*group)(nil)
)
