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

// Package idlookup finds a replicated document by its _id.
package idlookup

import (
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// FindDocument returns the document whose _id equals id. The id comes from a
// replication peer, so it is rejected if mongod would refuse to store it and is
// otherwise compared as a literal value, never evaluated as query operators.
func FindDocument(ctx context.Context, collection backends.Collection, id any) (*types.Document, bool, error) {
	if err := types.ValidateID(id); err != nil {
		return nil, false, fmt.Errorf("invalid replicated _id %v: %w", id, err)
	}
	filter := must.NotFail(types.NewDocument("_id", must.NotFail(types.NewDocument("$eq", id))))
	result, err := collection.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, false, err
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		candidate, candidateErr := document.Get("_id")
		if candidateErr == nil && types.Compare(candidate, id) == types.Equal {
			return document, true, nil
		}
	}
}
