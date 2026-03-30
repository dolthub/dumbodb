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
	"time"

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
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
}

// newSortByCount creates a new $sortByCount stage.
func newSortByCount(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$sortByCount")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Validate: null is not allowed.
	if _, isNull := v.(types.NullType); isNull {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$sortByCount requires a non-null expression argument",
			"$sortByCount (stage)",
		)
	}

	return &sortByCount{groupExpression: v}, nil
}

// Process implements Stage interface.
func (s *sortByCount) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all documents.
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Build a count per group-id.
	var m sortByCountMap

	for _, doc := range docs {
		id, err := evaluateGroupExpr(s.groupExpression, doc)
		if err != nil {
			id = types.Null
		}

		m.increment(id)
	}

	// Convert to result slice and sort by count desc, then _id asc for ties.
	res := make([]*types.Document, 0, len(m.entries))

	for _, e := range m.entries {
		res = append(res, must.NotFail(types.NewDocument("_id", e.id, "count", e.count)))
	}

	stdsort.Slice(res, func(i, j int) bool {
		ci := must.NotFail(res[i].Get("count")).(int32)
		cj := must.NotFail(res[j].Get("count")).(int32)

		if ci != cj {
			return ci > cj
		}

		// Break ties by _id ascending to match MongoDB spec.
		idi := must.NotFail(res[i].Get("_id"))
		idj := must.NotFail(res[j].Get("_id"))

		return types.CompareForAggregation(idi, idj) == types.Less
	})

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// evaluateGroupExpr evaluates a group expression (string field path, operator doc, or literal).
func evaluateGroupExpr(groupExpr any, doc *types.Document) (any, error) {
	switch expr := groupExpr.(type) {
	case *types.Document:
		if operators.IsOperator(expr) {
			op, err := operators.NewOperator(expr)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			return op.Process(doc)
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

// sortByCountEntry holds a group id and its count.
type sortByCountEntry struct {
	id    any
	count int32
}

// sortByCountMap accumulates counts per group id.
type sortByCountMap struct {
	entries []sortByCountEntry
}

// increment adds 1 to the count for the given id, creating a new entry if needed.
func (m *sortByCountMap) increment(id any) {
	for i, e := range m.entries {
		if types.CompareForAggregation(id, e.id) == types.Equal {
			m.entries[i].count++
			return
		}
	}

	m.entries = append(m.entries, sortByCountEntry{id: id, count: 1})
}

// check interfaces
var (
	_ aggregations.Stage = (*sortByCount)(nil)
)
