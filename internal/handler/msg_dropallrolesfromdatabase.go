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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgDropAllRolesFromDatabase(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	coll, err := h.systemRolesCollection()
	if err != nil {
		return nil, err
	}

	qr, err := coll.Query(connCtx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	defer qr.Iter.Close()

	var ids []any
	for {
		_, doc, err := qr.Iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if db, _ := doc.Get("db"); db == dbName {
			ids = append(ids, must.NotFail(doc.Get("_id")))
		}
	}

	var deleted int32
	if len(ids) > 0 {
		res, err := coll.DeleteAll(connCtx, &backends.DeleteAllParams{IDs: ids})
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		deleted = res.Deleted
	}

	h.BumpAuthGeneration()

	return documentOpMsg(must.NotFail(types.NewDocument(
		"n", deleted,
		"ok", float64(1),
	)))
}
