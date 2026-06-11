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

package handler

import (
	"context"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

// validateHintExists returns MongoDB's error when hint names an index that does
// not exist on the collection. A nil, empty, or $natural hint is always
// accepted. Matches MongoDB, which fails such a query rather than silently
// ignoring the hint and falling back to default index selection.
func validateHintExists(ctx context.Context, coll backends.Collection, hint any, command string) error {
	if !backends.HintRequiresExistingIndex(hint) {
		return nil
	}

	idxRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		// The collection does not exist (or its indexes are otherwise
		// unavailable). MongoDB does not validate a hint against a
		// non-existent collection -- the query simply returns no documents --
		// so skip validation here and let the query itself proceed.
		return nil //nolint:nilerr // intentionally skip hint validation when indexes are unavailable
	}

	if backends.MatchHintedIndex(hint, idxRes.Indexes) == "" {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"hint provided does not correspond to an existing index",
			command,
		)
	}

	return nil
}
