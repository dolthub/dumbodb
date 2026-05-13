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
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// count represents $count stage.
type count struct {
	field string
}

func newCount(stage *types.Document) (aggregations.Stage, error) {
	field, err := common.GetRequiredParam[string](stage, "$count")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageCountNonString,
			"the count field must be a non-empty string",
			"$count (stage)",
		)
	}

	if len(field) == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageCountNonEmptyString,
			"the count field must be a non-empty string",
			"$count (stage)",
		)
	}

	if strings.Contains(field, ".") {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageCountBadValue,
			"the count field cannot contain '.'",
			"$count (stage)",
		)
	}

	if strings.HasPrefix(field, "$") {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageCountBadPrefix,
			"the count field cannot be a $-prefixed path",
			"$count (stage)",
		)
	}

	if field == "_id" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupID,
			"a group's _id may only be specified once",
			"$count (stage)",
		)
	}

	return &count{
		field: field,
	}, nil
}

func (c *count) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return common.CountIterator(iter, closer, c.field), nil
}

var (
	_ aggregations.Stage = (*count)(nil)
)
