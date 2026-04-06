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
	"github.com/google/uuid"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/handler/common"
	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/handler/handlerparams"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/lazyerrors"
	"github.com/dolthub/docudolt/internal/util/must"
)

// MsgValidate implements `validate` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgValidate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "full", "repair", "metadata", "checkBSONConformance")

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	collectionVal, _ := document.Get(command)
	collection, ok := collectionVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			fmt.Sprintf("collection name has invalid type %s", handlerparams.AliasFromType(collectionVal)),
			command,
		)
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Get collection info to check for views and UUID.
	var isView bool
	var uuidBinary types.Binary
	if collsRes, collsErr := db.ListCollections(connCtx, &backends.ListCollectionsParams{Name: collection}); collsErr == nil {
		for _, ci := range collsRes.Collections {
			if ci.Name == collection {
				isView = ci.IsView
				if ci.UUID != "" {
					if collUUID, parseErr := uuid.Parse(ci.UUID); parseErr == nil {
						uuidBinary = types.Binary{
							Subtype: types.BinaryUUID,
							B:       must.NotFail(collUUID.MarshalBinary()),
						}
					}
				}
			}
		}
	}

	// For views, return a simplified validate response indicating view type.
	if isView {
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ns", dbName+"."+collection,
				"uuid", uuidBinary,
				"nInvalidDocuments", int32(0),
				"nNonCompliantDocuments", int32(0),
				"nrecords", int32(0),
				"nIndexes", int32(0),
				"keysPerIndex", must.NotFail(types.NewDocument()),
				"indexDetails", must.NotFail(types.NewDocument()),
				"valid", true,
				"repaired", false,
				"warnings", types.MakeArray(0),
				"errors", types.MakeArray(0),
				"extraIndexEntries", types.MakeArray(0),
				"missingIndexEntries", types.MakeArray(0),
				"corruptRecords", types.MakeArray(0),
				"ok", float64(1),
			)),
		)
	}

	c, err := db.Collection(collection)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	stats, err := c.Stats(connCtx, &backends.CollectionStatsParams{Refresh: true})
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			msg := fmt.Sprintf("Collection '%s.%s' does not exist to validate.", dbName, collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNamespaceNotFound, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	// Get indexes for keysPerIndex and indexDetails.
	indexRes, err := c.ListIndexes(connCtx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	keysPerIndex := must.NotFail(types.NewDocument())
	indexDetails := must.NotFail(types.NewDocument())

	for _, idx := range indexRes.Indexes {
		keysPerIndex.Set(idx.Name, int32(stats.CountDocuments))
		indexDetails.Set(idx.Name, must.NotFail(types.NewDocument("valid", true)))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ns", dbName+"."+collection,
			"uuid", uuidBinary,
			"nInvalidDocuments", int32(0),
			"nNonCompliantDocuments", int32(0),
			"nrecords", int32(stats.CountDocuments),
			"nIndexes", int32(len(indexRes.Indexes)),
			"keysPerIndex", keysPerIndex,
			"indexDetails", indexDetails,
			"valid", true,
			"repaired", false,
			"warnings", types.MakeArray(0),
			"errors", types.MakeArray(0),
			"extraIndexEntries", types.MakeArray(0),
			"missingIndexEntries", types.MakeArray(0),
			"corruptRecords", types.MakeArray(0),
			"ok", float64(1),
		)),
	)
}
