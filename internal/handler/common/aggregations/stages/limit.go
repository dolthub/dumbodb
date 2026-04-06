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

	"github.com/dolthub/docudolt/internal/handler/common"
	"github.com/dolthub/docudolt/internal/handler/common/aggregations"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/iterator"
	"github.com/dolthub/docudolt/internal/util/lazyerrors"
)

// limit represents $limit stage.
type limit struct {
	limit int64
}

// newLimit creates a new $limit stage.
func newLimit(stage *types.Document) (aggregations.Stage, error) {
	doc, err := stage.Get("$limit")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	l, err := common.GetLimitStageParam(doc)
	if err != nil {
		return nil, err
	}

	return &limit{
		limit: l,
	}, nil
}

// Process implements Stage interface.
func (l *limit) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return common.LimitIterator(iter, closer, l.limit), nil
}

// check interfaces
var (
	_ aggregations.Stage = (*limit)(nil)
)
