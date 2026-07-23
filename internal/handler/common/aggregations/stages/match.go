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
}

func newMatch(stage *types.Document) (aggregations.Stage, error) {
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

	return &match{
		filter: filter,
	}, nil
}

func (m *match) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return common.FilterIterator(iter, closer, m.filter), nil
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
