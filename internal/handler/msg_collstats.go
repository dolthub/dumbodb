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

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgCollStats implements `collStats` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgCollStats(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	collection, err := common.GetRequiredParam[string](document, command)
	if err != nil {
		return nil, err
	}

	scale := int64(1)

	var s any
	if s, err = document.Get("scale"); err == nil {
		if scale, err = handlerparams.GetValidatedNumberParamWithMinValue(command, "scale", s, 1); err != nil {
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

	c, err := db.Collection(collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	collectionParam := backends.ListCollectionsParams{Name: collection}
	collections, err := db.ListCollections(connCtx, &collectionParam)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var found bool
	var cInfo backends.CollectionInfo

	found = len(collections.Collections) > 0
	if found {
		cInfo = collections.Collections[0]
	}

	indexes, err := c.ListIndexes(connCtx, new(backends.ListIndexesParams))
	if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
		indexes = new(backends.ListIndexesResult)
		err = nil
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	stats, err := c.Stats(connCtx, &backends.CollectionStatsParams{Refresh: true})
	if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
		stats = new(backends.CollectionStatsResult)
		err = nil
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Build indexSizes document.
	indexSizes := types.MakeDocument(len(stats.IndexSizes))
	for _, indexSize := range stats.IndexSizes {
		indexSizes.Set(indexSize.Name, int32(indexSize.Size/scale))
	}

	// Build indexDetails document (one entry per index, value is an empty document).
	indexDetails := types.MakeDocument(len(indexes.Indexes))
	for _, idx := range indexes.Indexes {
		indexDetails.Set(idx.Name, must.NotFail(types.NewDocument()))
	}

	var pairs []any

	if !found {
		// Collection does not exist in the backend: return minimal stats.
		pairs = []any{
			"ns", dbName + "." + collection,
			"size", int32(stats.SizeCollection / scale),
			"count", int32(stats.CountDocuments),
			"numOrphanDocs", int32(0),
			"storageSize", int32(stats.SizeCollection / scale),
			"totalSize", int32(stats.SizeTotal / scale),
			"nindexes", int32(len(indexes.Indexes)),
			"totalIndexSize", int32(stats.SizeIndexes / scale),
			"indexDetails", indexDetails,
			"indexSizes", indexSizes,
			"scaleFactor", int32(scale),
			"ok", float64(1),
		}
	} else {
		// Collection exists: return full stats with freeStorageSize, capped, indexBuilds, etc.
		pairs = []any{
			"ns", dbName + "." + collection,
			"size", int32(stats.SizeCollection / scale),
			"count", int32(stats.CountDocuments),
		}

		if stats.CountDocuments > 0 {
			pairs = append(pairs, "avgObjSize", int32(stats.SizeCollection/stats.CountDocuments))
		}

		pairs = append(pairs,
			"numOrphanDocs", int32(0),
			"storageSize", int32(stats.SizeCollection/scale),
			"freeStorageSize", int32(stats.SizeFreeStorage/scale),
			"capped", cInfo.Capped(),
			"nindexes", int32(len(indexes.Indexes)),
			"indexDetails", indexDetails,
			"indexBuilds", must.NotFail(types.NewArray()),
			"totalIndexSize", int32(stats.SizeIndexes/scale),
			"indexSizes", indexSizes,
			"totalSize", int32(stats.SizeTotal/scale),
			"scaleFactor", int32(scale),
		)

		if cInfo.Capped() {
			pairs = append(pairs,
				"max", cInfo.CappedDocuments,
				"maxSize", cInfo.CappedSize/scale,
			)
		}

		pairs = append(pairs, "ok", float64(1))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(pairs...)),
	)
}
