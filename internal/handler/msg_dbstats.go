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
	"errors"
	"fmt"
	"math"
	"syscall"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgDBStats implements `dbStats` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDBStats(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	scale := int64(1)

	var s any
	if s, err = document.Get("scale"); err == nil {
		if scale, err = dbStatsGetScale(command, s); err != nil {
			return nil, err
		}
	}

	var freeStorage bool

	if v, _ := document.Get("freeStorage"); v != nil {
		if freeStorage, err = handlerparams.GetBoolOptionalParam("freeStorage", v); err != nil {
			return nil, err
		}
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid database specified '%s'", dbName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	list, err := db.ListCollections(connCtx, new(backends.ListCollectionsParams))
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var nIndexes int64

	for _, cInfo := range list.Collections {
		var c backends.Collection

		c, err = db.Collection(cInfo.Name)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var iList *backends.ListIndexesResult

		iList, err = c.ListIndexes(connCtx, new(backends.ListIndexesParams))
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
			iList = new(backends.ListIndexesResult)
			err = nil
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		nIndexes += int64(len(iList.Indexes))
	}

	stats, err := db.Stats(connCtx, &backends.DatabaseStatsParams{Refresh: true})
	if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
		stats = new(backends.DatabaseStatsResult)
		err = nil
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var avgObjSize float64
	if stats.CountDocuments > 0 {
		avgObjSize = float64(stats.SizeCollections) / float64(stats.CountDocuments)
	}

	pairs := []any{
		"db", dbName,
		"collections", int64(len(list.Collections)),
		// TODO https://github.com/dolthub/dongo/issues/176
		"views", int64(0),
		"objects", stats.CountDocuments,
		"avgObjSize", avgObjSize,
		"dataSize", float64(stats.SizeCollections) / float64(scale),
		"storageSize", float64(stats.SizeCollections) / float64(scale),
		"indexes", nIndexes,
		"indexSize", float64(stats.SizeIndexes) / float64(scale),
		"totalSize", float64(stats.SizeTotal) / float64(scale),
	}

	if freeStorage {
		pairs = append(pairs,
			"freeStorageSize", float64(stats.SizeFreeStorage)/float64(scale),
			"indexFreeStorageSize", float64(0),
			"totalFreeStorageSize", float64(stats.SizeFreeStorage)/float64(scale),
		)
	}

	var fsStat syscall.Statfs_t
	var fsUsedSize, fsTotalSize float64
	if (stats.CountDocuments > 0 || stats.SizeTotal > 0) && syscall.Statfs("/", &fsStat) == nil {
		fsTotalSize = float64(fsStat.Blocks) * float64(fsStat.Bsize)
		fsUsedSize = fsTotalSize - float64(fsStat.Bfree)*float64(fsStat.Bsize)
	}

	pairs = append(pairs,
		"scaleFactor", scale,
		"fsUsedSize", fsUsedSize,
		"fsTotalSize", fsTotalSize,
		"ok", float64(1),
	)

	return documentOpMsg(
		must.NotFail(types.NewDocument(pairs...)),
	)
}

// dbStatsGetScale validates and returns the scale parameter for dbStats.
// MongoDB returns BadValue (code 2) for negative/zero scale, unlike collStats which returns Location51024.
func dbStatsGetScale(command string, value any) (int64, error) {
	whole, err := handlerparams.GetWholeNumberParam(value)
	if err != nil {
		switch {
		case errors.Is(err, handlerparams.ErrUnexpectedType):
			if _, ok := value.(types.NullType); ok {
				return 1, nil
			}

			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf(
					`BSON field '%s.scale' is the wrong type '%s', expected types '[long, int, decimal, double']`,
					command, handlerparams.AliasFromType(value),
				),
				command,
			)
		case errors.Is(err, handlerparams.ErrNotWholeNumber):
			if math.Signbit(value.(float64)) {
				return 0, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"Scale factor must be greater than zero",
					command,
				)
			}
			// non-integer positive numbers: round down
			return int64(math.Floor(value.(float64))), nil
		case errors.Is(err, handlerparams.ErrLongExceededPositive):
			return math.MaxInt32, nil
		case errors.Is(err, handlerparams.ErrLongExceededNegative):
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"Scale factor must be greater than zero",
				command,
			)
		default:
			return 0, lazyerrors.Error(err)
		}
	}

	if whole < 1 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Scale factor must be greater than zero",
			command,
		)
	}

	if whole > math.MaxInt32 {
		return math.MaxInt32, nil
	}

	return whole, nil
}
