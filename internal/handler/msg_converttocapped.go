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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgConvertToCapped implements `convertToCapped` command.
//
// convertToCapped converts a regular collection to a capped collection by
// marking it with the specified size limit. The data is preserved.
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

	// size is required and must be > 0.
	sizeVal, sizeErr := document.Get("size")
	var cappedSize int64

	if sizeErr != nil || sizeVal == nil || sizeVal == types.Null {
		// Missing size field.
		cappedSize = 0
	} else {
		cappedSize, err = handlerparams.GetWholeNumberParam(sizeVal)
		if err != nil {
			cappedSize = 0
		}
	}

	if cappedSize <= 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidOptions,
			"Capped collection size must be greater than zero",
			command,
		)
	}

	// Match MongoDB: enforce a 4096-byte minimum for capped collections and
	// round larger sizes up to the next 256-byte multiple.
	cappedSize = normalizeCappedSize(cappedSize)

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid database specified '%s'", dbName),
				command,
			)
		}
		return nil, lazyerrors.Error(err)
	}

	// Update the collection to be capped by modifying its options.
	if err = db.CollMod(connCtx, &backends.CollModParams{
		Name:       collectionName,
		CappedSize: cappedSize,
	}); err != nil {
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
		case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid collection name: %s", collectionName),
				command,
			)
		default:
			return nil, lazyerrors.Error(err)
		}
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
