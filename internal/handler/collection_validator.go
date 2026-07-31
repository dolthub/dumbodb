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
	"github.com/dolthub/dumbodb/internal/types"
)

// collectionValidator returns the active document validator for a collection,
// read from its durable metadata. Returns (nil, "", "") when no validator
// applies -- the collection is absent, is a view, has no validator, or has
// validationLevel "off". Otherwise the returned action is normalized to "error"
// when unset. Used by every write path (insert/update/findAndModify/bulkWrite)
// so enforcement is uniform.
func collectionValidator(ctx context.Context, db backends.Database, collName string) (validator *types.Document, level, action string) {
	collRes, err := db.ListCollections(ctx, &backends.ListCollectionsParams{Name: collName})
	if err != nil || len(collRes.Collections) != 1 {
		return nil, "", ""
	}
	ci := collRes.Collections[0]
	if ci.IsView || ci.Validator == nil || ci.ValidationLevel == "off" {
		return nil, "", ""
	}
	action = ci.ValidationAction
	if action == "" {
		action = "error"
	}
	return ci.Validator, ci.ValidationLevel, action
}
