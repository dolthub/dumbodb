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
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
)

// TestAutoCommitRecording verifies that under --auto-commit a write records the
// touched (db, branch) and a proposed message on the connection's ConnInfo, for
// the command boundary to drain and commit.
func TestAutoCommitRecording(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-autocommit-record-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	ctx := context.Background()
	b := &Backend{
		dataDir:    dir,
		l:          slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:        make(map[string]*dbState),
		autoCommit: true,
	}
	defer b.Close()

	_, err = b.getOrOpenDB(ctx, "testdb", true)
	require.NoError(t, err)

	ci := conninfo.New()
	cctx := conninfo.Ctx(ctx, ci)

	db, err := b.Database("testdb")
	require.NoError(t, err)
	coll, err := db.Collection("col")
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", int64(1))
	require.NoError(t, err)
	doc.SetRecordID(1)
	_, err = coll.InsertAll(cctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)

	targets := ci.DrainAutoCommit()
	require.Len(t, targets, 1, "one write must record exactly one branch")
	require.Equal(t, "testdb", targets[0].DB)
	require.Equal(t, defaultBranch, targets[0].Branch)
	require.Equal(t, "auto: insert 1 docs into col", targets[0].Message)

	// Drain is one-shot: a second drain returns nothing.
	require.Empty(t, ci.DrainAutoCommit())
}
