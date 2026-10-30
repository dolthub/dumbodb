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

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// match represents $match stage.
type match struct {
	filter *types.Document
	cmp    *collation.Comparator
}

func newMatch(stage *types.Document) (aggregations.Stage, error) {
	return NewMatchStage(stage, nil, true)
}

// NewMatchStage builds a $match stage whose string comparisons honor cmp when
// non-nil. storedInput reports whether the stage sees documents as stored
// (see SeesStoredDocuments); only then are the document values it compares
// against put in stored key order.
func NewMatchStage(stage *types.Document, cmp *collation.Comparator, storedInput bool) (aggregations.Stage, error) {
	filter, err := common.GetRequiredParam[*types.Document](stage, "$match")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMatchBadExpression,
			"the match filter must be an expression in an object",
			"$match (stage)",
		)
	}

	if err := validateMatch(filter); err != nil {
		return nil, err
	}

	if storedInput {
		common.CanonicalizeFilter(filter)
	}
	return &match{filter: filter, cmp: cmp}, nil
}

func (m *match) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return common.FilterIteratorColl(iter, closer, m.filter, m.cmp), nil
}

// geoNearNotAllowedMsg is MongoDB's verbatim Location5626500 message
// (including its "operationfor" typo) so the error compares equal.
const geoNearNotAllowedMsg = "$geoNear, $near, and $nearSphere are not allowed in this context, " +
	"as these operators require sorting geospatial data. If you do not need sort, consider using " +
	"$geoWithin instead. Check out https://dochub.mongodb.org/core/near-sort-operation and " +
	"https://dochub.mongodb.org/core/nearSphere-sort-operationfor more details."

// validateMatch validates $expr field if any.
func validateMatch(filter *types.Document) error {
	if filter.Has("$expr") {
		_, err := operators.NewExpr(filter, "$match (stage)")
		if err != nil {
			return err
		}
	}

	// $near / $nearSphere require a sort context, so they are not permitted in
	// a $match stage (the path used by count and general aggregation). MongoDB
	// rejects them here with Location5626500.
	if common.FindGeoSortKey(filter) != nil {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrGeoNearNotAllowedInContext,
			geoNearNotAllowedMsg,
		)
	}

	return nil
}

var (
	_ aggregations.Stage = (*match)(nil)
)

// SeesStoredDocuments reports whether stage i of pipeline receives documents
// exactly as stored: every stage before it passes its input on unchanged.
// Stored documents have sorted keys; documents a stage builds keep the order
// they were built in.
func SeesStoredDocuments(pipeline []any, i int) bool {
	for _, stage := range pipeline[:i] {
		doc, ok := stage.(*types.Document)
		if !ok || doc.Len() == 0 {
			return false
		}
		switch doc.Command() {
		case "$match", "$sort", "$limit", "$skip", "$sample":
		default:
			return false
		}
	}
	return true
}
