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

// sort represents $sort stage. limit is the top-K bound when a $limit (or
// $skip + $limit) immediately follows; 0 means an unbounded full sort.
type sort struct {
	fields *types.Document
	limit  int64
	cmp    *collation.Comparator
}

// wholeIDKey holds [_id] while a sort on _id runs, so the sort takes the
// array's one element, _id itself. No client field name has a NUL byte.
const wholeIDKey = "\x00_id"

func newSort(stage *types.Document) (aggregations.Stage, error) {
	return NewSortStage(stage, 0, nil)
}

// NewSortStage builds a $sort stage that keeps only the first limit documents
// (in sort order) when limit > 0, bounding memory for a following $limit.
// String ordering honors cmp when non-nil.
func NewSortStage(stage *types.Document, limit int64, cmp *collation.Comparator) (aggregations.Stage, error) {
	if cmp == nil {
		cmp = operators.CollationOf(stage)
	}
	fields, err := common.GetRequiredParam[*types.Document](stage, "$sort")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrSortBadExpression,
			"the $sort key specification must be an object",
			"$sort (stage)",
		)
	}

	if fields.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrSortMissingKey,
			"$sort stage must have at least one sort key",
			"$sort (stage)",
		)
	}

	return &sort{
		fields: fields,
		limit:  limit,
		cmp:    cmp,
	}, nil
}

// Process implements Stage interface.
//
// If sort path is invalid, it returns a possibly wrapped types.PathError.
func (s *sort) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// mongod assumes _id is never an array, as in stored documents, so a
	// sort on _id alone compares whole values, arrays included.
	fields := s.fields
	wholeID := fields.Len() == 1 && fields.Has("_id")
	if wholeID {
		fields = types.MakeDocument(s.fields.Len())
		for _, k := range s.fields.Keys() {
			name := k
			if k == "_id" {
				name = wholeIDKey
			}
			fields.Set(name, must.NotFail(s.fields.Get(k)))
		}
		iter = newMapIterator(iter, closer, func(doc *types.Document) (*types.Document, error) {
			id, err := doc.Get("_id")
			if err != nil {
				id = types.Null
			}
			// A shallow copy, as another stage, such as a $facet branch,
			// may share doc.
			keyed := types.MakeDocument(doc.Len() + 1)
			keys, values := doc.Keys(), doc.Values()
			for i, k := range keys {
				keyed.AppendDecoded(k, values[i])
			}
			keyed.SetRecordID(doc.RecordID())
			keyed.Set(wholeIDKey, must.NotFail(types.NewArray(id)))
			return keyed, nil
		})
	}
	iter, err := common.TopKIteratorColl(iter, closer, fields, s.limit, s.cmp)
	if err == nil && wholeID {
		iter = newMapIterator(iter, closer, func(doc *types.Document) (*types.Document, error) {
			doc.Remove(wholeIDKey)
			return doc, nil
		})
	}
	if err != nil {
		var pathErr *types.PathError
		if errors.As(err, &pathErr) && pathErr.Code() == types.ErrPathElementEmpty {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrPathContainsEmptyElement,
				"FieldPath field names may not be empty strings.",
				"$sort (stage)",
			)
		}

		return nil, lazyerrors.Error(err)
	}

	return iter, nil
}

var (
	_ aggregations.Stage = (*sort)(nil)
)
