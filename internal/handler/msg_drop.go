// Copyright 2021 FerretDB Inc.
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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDrop implements `drop` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDrop(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document); err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	if err = enforceWritableRootish(dbName); err != nil {
		return nil, err
	}

	collectionName, err := common.GetRequiredParam[string](document, command)
	if err != nil {
		return nil, err
	}

	// Most backends would block on `DropCollection` below otherwise.
	//
	// There is a race condition: another client could create a new cursor for that collection
	// after we closed all of them, but before we drop the collection itself.
	// In that case, we expect the client to wait or to retry the operation.
	for _, c := range h.cursors.All() {
		if c.DB == dbName && c.Collection == collectionName {
			h.cursors.CloseAndRemove(c)
		}
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s'", dbName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "drop")
		}

		return nil, lazyerrors.Error(err)
	}

	// Count indexes before dropping so we can include nIndexesWas in the
	// response, matching MongoDB behavior.
	var nIndexesWas int32 = 1 // every collection has at least the _id index

	c, cErr := db.Collection(collectionName)
	if cErr == nil {
		if idxRes, idxErr := c.ListIndexes(connCtx, nil); idxErr == nil {
			nIndexesWas = int32(len(idxRes.Indexes))
		}
	}

	err = db.DropCollection(connCtx, &backends.DropCollectionParams{
		Name: collectionName,
	})

	switch {
	case err == nil:
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"nIndexesWas", nIndexesWas,
				"ns", dbName+"."+collectionName,
				"ok", float64(1),
			)),
		)

	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist):
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ok", float64(1),
			)),
		)

	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
		msg := fmt.Sprintf("Invalid collection name: %s", collectionName)
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "drop")

	default:
		return nil, common.TranslateBackendWriteError(err)
	}
}
