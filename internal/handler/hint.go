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
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
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
		// MongoDB does not validate a hint against a non-existent namespace
		// (the query just returns no documents), so skip validation when the
		// collection or database does not exist. Any other error is a real
		// backend failure and must propagate rather than letting an
		// unvalidated hint through.
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			return nil
		}
		return lazyerrors.Error(err)
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

// hintedDescendingOrder returns the key pattern of the index hint names when
// it has a descending key, else nil. An unsorted hinted read returns index
// order, but DumboDB stores every key ascending, so such a read sorts by the
// pattern; the sort is stable over _id order, as mongod's index orders equal
// keys by RecordId.
func hintedDescendingOrder(ctx context.Context, coll backends.Collection, hint any) (*types.Document, error) {
	if !backends.HintRequiresExistingIndex(hint) {
		return nil, nil
	}
	idxRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		return nil, nil
	}
	name := backends.MatchHintedIndex(hint, idxRes.Indexes)
	for _, idx := range idxRes.Indexes {
		if idx.Name != name {
			continue
		}
		descending := false
		order := types.MakeDocument(len(idx.Key))
		for _, k := range idx.Key {
			if k.Text || k.Geo2D || k.Geo2DSphere || k.Hashed {
				return nil, nil
			}
			dir := int32(1)
			if k.Descending {
				dir, descending = -1, true
			}
			order.Set(k.Field, dir)
		}
		if descending {
			return order, nil
		}
	}
	return nil, nil
}
