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
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
)

// effectiveCollation applies MongoDB's operation > collection-default > simple
// precedence: it returns opCollation when the operation specified one (including
// an explicit {locale:"simple"} opting down to binary), otherwise the
// collection's persisted default, otherwise nil (simple). The collection default
// is only looked up when the operation carried none.
func (h *Handler) effectiveCollation(ctx context.Context, db backends.Database, collName string, opCollation *types.Document) *types.Document {
	if opCollation != nil {
		return opCollation
	}
	cInfo, err := lookupCollectionInfo(ctx, db, collName)
	if err != nil || cInfo == nil {
		return nil
	}
	return collation.Effective(nil, cInfo.Collation)
}
