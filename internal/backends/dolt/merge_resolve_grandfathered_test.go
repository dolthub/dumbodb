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
)

// Resolving a conflict with ours keeps the document ours already has, so it is not a write and is not validated,
// even when it fails the validator. The history merges feat into main:
//
//	c0 -- m1    main
//	  \-- f1    feat
//
// c0 has document 1 {v: 1}. m1 adds a moderate validator requiring w, which document 1 predates, and sets its v to
// 2, which a moderate validator allows for a document that already fails it. f1 sets v to 3.
func TestResolvingWithOursKeepsADocumentThatFailsTheValidator(t *testing.T) {
	for _, mode := range mergeModes {
		t.Run(mode, func(t *testing.T) {
			b := newTestBackend(t)
			ctx := context.Background()
			createItems(t, b, mode)
			insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "v", int32(1)))
			commitDB(t, b, "testdb", "c0")
			branchFrom(t, b, "testdb", "main", "feat")
			collModOn(t, b, "main", &backends.CollModParams{
				Name: "items", Validator: requireExists(t, "w"), SetValidator: true, ValidationLevel: "moderate",
			})
			updateDocOnBranch(t, b, "testdb", "main", "items", mustDoc(t, "_id", int32(1), "v", int32(2)))
			commitBranch(t, b, "testdb", "main", "m1")
			updateDocOnBranch(t, b, "testdb", "feat", "items", mustDoc(t, "_id", int32(1), "v", int32(3)))
			commitBranch(t, b, "testdb", "feat", "f1")

			_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "main", From: "feat", Message: "merge feat", Author: "testuser <test@example.com>",
			})
			require.Error(t, err, "the merge must conflict on document 1")
			c := conflictOnDoc(t, b, "main", 1)
			require.Error(t, resolveConflict(t, b, "testdb", "main", "items", c.ConflictID, "theirs", nil),
				"theirs writes a document that fails the validator")
			require.NoError(t, resolveConflict(t, b, "testdb", "main", "items", c.ConflictID, "ours", nil))
			_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: "testdb", Into: "main", Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
			})
			require.NoError(t, err)
			requireSameDoc(t, mustDoc(t, "_id", int32(1), "v", int32(2)), virtualBaseItems(t, b, branchHash(t, b, "main"))[1])
		})
	}
}
