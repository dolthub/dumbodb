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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// unreadableDBBackend lists one database whose stats cannot be read, as one
// an older version wrote.
type unreadableDBBackend struct{ backends.Backend }

func (b unreadableDBBackend) ListDatabases(context.Context, *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	return &backends.ListDatabasesResult{Databases: []backends.DatabaseInfo{{Name: "olddb"}}}, nil
}

func (b unreadableDBBackend) Database(name string) (backends.Database, error) {
	db, err := b.Backend.Database(name)
	return unreadableDB{db}, err
}

type unreadableDB struct{ backends.Database }

func (unreadableDB) Stats(context.Context, *backends.DatabaseStatsParams) (*backends.DatabaseStatsResult, error) {
	return nil, errors.New("a storage layout this version does not read")
}

func TestListDatabasesKeepsUnreadableDatabase(t *testing.T) {
	h := authGateHandler(t, false)
	h.b = unreadableDBBackend{h.b}

	res, err := h.commands["listDatabases"].Handler(conninfo.Ctx(context.Background(), conninfo.New()), gateCmd(t, "listDatabases"))
	require.NoError(t, err)
	doc, err := opMsgDocument(res)
	require.NoError(t, err)
	dbs := must.NotFail(doc.Get("databases")).(*types.Array)
	require.Equal(t, 1, dbs.Len())
	db := must.NotFail(dbs.Get(0)).(*types.Document)
	require.Equal(t, "olddb", must.NotFail(db.Get("name")))
	require.Equal(t, false, must.NotFail(db.Get("empty")))
}
