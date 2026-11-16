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

// dropPendingBackend refuses every write the way the backend refuses one to a
// database being dropped.
type dropPendingBackend struct{ backends.Backend }

type dropPendingDatabase struct{ backends.Database }

type dropPendingCollection struct{ backends.Collection }

func errDropPending() error {
	return backends.NewError(backends.ErrorCodeDatabaseDropPending, errors.New("database is in the process of being dropped"))
}

func (b dropPendingBackend) Database(name string) (backends.Database, error) {
	db, err := b.Backend.Database(name)
	return dropPendingDatabase{db}, err
}

func (db dropPendingDatabase) Collection(name string) (backends.Collection, error) {
	c, err := db.Database.Collection(name)
	return dropPendingCollection{c}, err
}

func (db dropPendingDatabase) CreateCollection(context.Context, *backends.CreateCollectionParams) error {
	return errDropPending()
}

func (c dropPendingCollection) InsertAll(context.Context, *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	return nil, errDropPending()
}

// mongod reports a write into a database being dropped as a 215 write error
// for each document, in an ok:1 reply, not as a failed command.
func TestInsertIntoDatabaseBeingDroppedIsWriteError(t *testing.T) {
	h := authGateHandler(t, false)
	h.b = dropPendingBackend{h.b}
	h.BatchSize = 100
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	res, err := h.MsgInsert(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"insert", "c",
		"documents", must.NotFail(types.NewArray(
			must.NotFail(types.NewDocument("_id", int32(1))),
			must.NotFail(types.NewDocument("_id", int32(2))),
		)),
		"ordered", false,
		"$db", "s3",
	)))))
	require.NoError(t, err)

	doc := must.NotFail(opMsgDocument(res))
	require.Equal(t, float64(1), must.NotFail(doc.Get("ok")))
	require.Equal(t, int32(0), must.NotFail(doc.Get("n")))
	writeErrors := must.NotFail(doc.Get("writeErrors")).(*types.Array)
	require.Equal(t, 2, writeErrors.Len())
	for i := range writeErrors.Len() {
		we := must.NotFail(writeErrors.Get(i)).(*types.Document)
		require.Equal(t, int32(i), must.NotFail(we.Get("index")))
		require.Equal(t, int32(215), must.NotFail(we.Get("code")))
		require.Equal(t, "Cannot create collection s3.c - database is in the process of being dropped.", must.NotFail(we.Get("errmsg")))
	}
}

// An upsert into a database being dropped reports 215 as its statement's
// write error too.
func TestUpsertIntoDatabaseBeingDroppedIsWriteError(t *testing.T) {
	h := authGateHandler(t, false)
	h.b = dropPendingBackend{h.b}
	h.BatchSize = 100
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	res, err := h.MsgUpdate(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"update", "c",
		"updates", must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
			"q", must.NotFail(types.NewDocument("_id", int32(1))),
			"u", must.NotFail(types.NewDocument("$set", must.NotFail(types.NewDocument("v", int32(1))))),
			"upsert", true,
		)))),
		"$db", "s3",
	)))))
	require.NoError(t, err)

	doc := must.NotFail(opMsgDocument(res))
	require.Equal(t, float64(1), must.NotFail(doc.Get("ok")))
	writeErrors := must.NotFail(doc.Get("writeErrors")).(*types.Array)
	require.Equal(t, 1, writeErrors.Len())
	we := must.NotFail(writeErrors.Get(0)).(*types.Document)
	require.Equal(t, int32(215), must.NotFail(we.Get("code")))
	require.Equal(t, "Cannot create collection s3.c - database is in the process of being dropped.", must.NotFail(we.Get("errmsg")))
}
