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
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// Every branch of a database shares its store, so dropping the database must
// close cursors opened on any of its branches, and only those.
func TestCloseDatabaseCursorsMatchesEveryBranch(t *testing.T) {
	h := &Handler{cursors: cursor.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))}
	t.Cleanup(func() {
		for _, c := range h.cursors.All() {
			h.cursors.CloseAndRemove(c)
		}
		h.cursors.Close()
	})

	open := func(db string) *cursor.Cursor {
		iter := iterator.Values(iterator.ForSlice([]*types.Document{}))
		return h.cursors.NewCursor(context.Background(), iter, &cursor.NewParams{
			Type: cursor.Normal, DB: db, Collection: "c",
		})
	}
	open("victim")
	open("victim@feature")
	kept := open("victimized")

	h.closeDatabaseCursors("victim")

	remaining := h.cursors.All()
	require.Len(t, remaining, 1)
	require.Equal(t, kept.ID, remaining[0].ID)
}
