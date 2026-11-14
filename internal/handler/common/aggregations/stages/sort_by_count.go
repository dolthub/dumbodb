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
	stdsort "sort"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sortByCount represents $sortByCount stage.
//
//	{ $sortByCount: <expression> }
//
// $sortByCount groups documents by expression and counts the number of documents in each group,
// then sorts by count descending. It is equivalent to:
//
//	[{$group: {_id: <expression>, count: {$sum: 1}}}, {$sort: {count: -1, _id: 1}}]
type sortByCount struct {
	groupExpression any
	cmp             *collation.Comparator
}

func newSortByCount(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$sortByCount")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	switch val := v.(type) {
	case string:
		if !strings.HasPrefix(val, "$") || val == "$" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortByCountString,
				"the sortByCount field must be defined as a $-prefixed path or an expression inside an object",
				"$sortByCount (stage)")
		}
	case *types.Document:
		if !isGroupExpression(val) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortByCountObject,
				"the sortByCount field must be defined as a $-prefixed path or an expression inside an object",
				"$sortByCount (stage)")
		}
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortByCountType,
			"the sortByCount field must be specified as a string or as an object", "$sortByCount (stage)")
	}

	if err := operators.CheckFieldPaths(v); err != nil {
		return nil, err
	}
	if err := operators.CheckConstantSubexpressions(v); err != nil {
		return nil, err
	}

	return &sortByCount{groupExpression: v, cmp: operators.CollationOf(stage)}, nil
}

func (s *sortByCount) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all documents.
	docs, err := common.ConsumeDocuments(iter, "$sortByCount")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Build a count per group-id.
	m := sortByCountMap{cmp: s.cmp}

	for _, doc := range docs {
		id, err := evaluateGroupExpr(s.groupExpression, doc)
		if err != nil {
			return nil, err
		}

		m.increment(id)
	}

	// Sort by count desc, then _id asc for ties, as
	// $group + $sort{count:-1, _id:1}.
	entries := m.entries
	stdsort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return types.CompareForAggregation(entries[i].key, entries[j].key) == types.Less
	})

	res := make([]*types.Document, 0, len(entries))
	for _, e := range entries {
		res = append(res, must.NotFail(types.NewDocument("_id", e.id, "count", e.count)))
	}

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// evaluateGroupExpr evaluates a group expression (string field path, operator doc, or literal).
func evaluateGroupExpr(groupExpr any, doc *types.Document) (any, error) {
	switch expr := groupExpr.(type) {
	case *types.Document:
		if operators.IsOperator(expr) {
			val, missing, err := operators.EvalOperand(expr, doc)
			switch {
			case err != nil:
				return nil, processGroupStageError(err)
			case missing:
				return types.Null, nil
			}
			return val, nil
		}

		return evaluateDocument(expr, doc, false)
	case string:
		expression, err := aggregations.NewExpression(expr, nil)

		var exprErr *aggregations.ExpressionError
		if errors.As(err, &exprErr) && exprErr.Code() == aggregations.ErrNotExpression {
			return expr, nil
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		val, err := expression.Evaluate(doc)
		if err != nil {
			return types.Null, nil
		}

		return val, nil
	case *types.Array, float64, types.Binary, types.ObjectID, bool, time.Time, types.NullType,
		types.Regex, int32, types.Timestamp, int64:
		return expr, nil
	default:
		return types.Null, nil
	}
}

type sortByCountEntry struct {
	id    any
	key   any
	count int32
}

// sortByCountMap accumulates counts per group id.
type sortByCountMap struct {
	entries []sortByCountEntry
	cmp     *collation.Comparator
}

// increment adds 1 to the count for the given id, creating a new entry if needed.
func (m *sortByCountMap) increment(id any) {
	key := m.cmp.KeyValue(id)
	for i, e := range m.entries {
		if types.CompareForAggregation(key, e.key) == types.Equal {
			m.entries[i].count++
			return
		}
	}

	m.entries = append(m.entries, sortByCountEntry{id: id, key: key, count: 1})
}

var (
	_ aggregations.Stage = (*sortByCount)(nil)
)
