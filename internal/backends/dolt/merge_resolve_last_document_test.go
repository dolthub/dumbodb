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

package dolt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// Resolving a merge's conflict on a collection's last document so that the document goes away leaves the collection
// empty, as deleting it would, with its metadata. Each test merges feat into main:
//
//	c0 -- m1    main
//	  \-- f1    feat
//
// c0 creates items, in fieldDivergent with a validator that refuses x and y together, holding document 1.
func TestResolvingTheLastDocumentAwayKeepsTheCollection(t *testing.T) {
	tests := []struct {
		name       string
		main, feat edit
		resolution string
	}{
		{"theirs deleted it", setTo(xyDoc("v", "main")), deleteItem(1), "theirs"},
		{"the merged document fails the validator", setTo(xyDoc("x", int32(1))), setTo(xyDoc("y", int32(1))), "drop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestBackend(t)
			ctx := context.Background()
			createValidatedItems(t, b, string(MergeModeFieldDivergent), "", notBothXAndY(t), xyDoc()(t))
			commitDB(t, b, "testdb", "c0")
			branchFrom(t, b, "testdb", "main", "feat")
			tt.main(t, b, "main")
			commitBranch(t, b, "testdb", "main", "m1")
			tt.feat(t, b, "feat")
			commitBranch(t, b, "testdb", "feat", "f1")

			_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "main", From: "feat", Message: "merge feat", Author: "testuser <test@example.com>",
			})
			require.Error(t, err, "the merge must conflict")
			c := conflictOnDoc(t, b, "main", 1)
			require.NoError(t, resolveConflict(t, b, "testdb", "main", "items", c.ConflictID, tt.resolution, nil))
			requireEmptyItemsWithMetadata(t, b)

			_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "main", Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
			})
			require.NoError(t, err)
			requireEmptyItemsWithMetadata(t, b)
			meta := virtualCatalog(t, b, branchHash(t, b, "main"), "items")
			require.NotNil(t, meta, "the merge commit must keep items")
			require.Equal(t, string(MergeModeFieldDivergent), meta.MergeMode)
			require.NotNil(t, meta.Validator)
		})
	}
}

func requireEmptyItemsWithMetadata(t *testing.T, b *Backend) {
	t.Helper()
	ctx := context.Background()
	db, err := b.Database("testdb")
	require.NoError(t, err)
	infos, err := db.ListCollections(ctx, &backends.ListCollectionsParams{})
	require.NoError(t, err)
	var items *backends.CollectionInfo
	for i := range infos.Collections {
		if infos.Collections[i].Name == "items" {
			items = &infos.Collections[i]
		}
	}
	require.NotNil(t, items, "items must still exist")
	require.Equal(t, string(MergeModeFieldDivergent), items.MergeMode)
	require.True(t, documentsEqual(notBothXAndY(t), items.Validator), "validator %v", items.Validator)
	require.Empty(t, drainQuery(t, ctx, collAt(t, b, "testdb", "main", "items"), &backends.QueryParams{}))
}

// xyDoc returns document 1 of items with c "seed" and |fields|.
func xyDoc(fields ...any) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document {
		return mustDoc(t, append([]any{"_id", int32(1), "c", "seed"}, fields...)...)
	}
}
