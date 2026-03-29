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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgConvertToCapped implements `convertToCapped` command.
//
// It converts an existing collection to a capped collection by dropping the
// original and recreating it with the specified capped size.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgConvertToCapped(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	collectionName, err := common.GetRequiredParam[string](document, command)
	if err != nil {
		return nil, err
	}

	// Validate the size parameter — required and must be > 0.
	sizeVal, sizeErr := document.Get("size")
	if sizeErr != nil || sizeVal == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidOptions,
			"Capped collection size must be greater than zero",
			command,
		)
	}

	size, err := handlerparams.GetWholeNumberParam(sizeVal)
	if err != nil || size <= 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidOptions,
			"Capped collection size must be greater than zero",
			command,
		)
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collectionName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, command)
		}

		return nil, lazyerrors.Error(err)
	}

	// Collect existing documents so they can be reinserted into the new capped collection.
	c, err := db.Collection(collectionName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid collection name: %s", collectionName),
				command,
			)
		}

		return nil, lazyerrors.Error(err)
	}

	// Verify the collection exists by fetching its stats.
	_, err = c.Stats(connCtx, new(backends.CollectionStatsParams))
	if err != nil {
		switch {
		case backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNamespaceNotFound,
				fmt.Sprintf("database %s not found", dbName),
				command,
			)
		case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNamespaceNotFound,
				fmt.Sprintf("source collection %s.%s does not exist", dbName, collectionName),
				command,
			)
		}

		return nil, lazyerrors.Error(err)
	}

	// Read all existing documents so they can be reinserted after conversion.
	qRes, err := c.Query(connCtx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var docs []*types.Document

	iter := qRes.Iter
	defer iter.Close()

	for {
		_, doc, iterErr := iter.Next()
		if iterErr != nil {
			break
		}

		docs = append(docs, doc)
	}

	// Drop the original collection and recreate it as capped.
	if err = db.DropCollection(connCtx, &backends.DropCollectionParams{Name: collectionName}); err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = db.CreateCollection(connCtx, &backends.CreateCollectionParams{
		Name:        collectionName,
		CappedSize:  size,
	}); err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Reinsert existing documents into the new capped collection.
	if len(docs) > 0 {
		newCol, colErr := db.Collection(collectionName)
		if colErr != nil {
			return nil, lazyerrors.Error(colErr)
		}

		if _, insertErr := newCol.InsertAll(connCtx, &backends.InsertAllParams{Docs: docs}); insertErr != nil {
			return nil, lazyerrors.Error(insertErr)
		}
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
