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
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDistinct implements `distinct` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDistinct(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetDistinctParams(document, h.L)
	if err != nil {
		return nil, err
	}

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(params.DB); err != nil {
		return nil, err
	}

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	viewInfo, err := lookupCollectionInfo(connCtx, db, params.Collection)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if viewInfo != nil && viewInfo.IsView {
		if params.Collation, err = viewReadCollation(params.Collation, viewInfo.Collation); err != nil {
			return nil, err
		}
		cmp := collation.Parse(params.Collation).Comparator()

		closer := iterator.NewMultiCloser()
		defer closer.Close()

		iter, verr := viewSourceIterator(connCtx, db, viewInfo.Name, viewInfo.ViewOn, viewInfo.ViewPipeline, cmp, closer, h.DisablePushdown, h.EnableNestedPushdown)
		if verr != nil {
			return nil, verr
		}
		iter = common.FilterIteratorColl(iter, closer, params.Filter, cmp)

		distinct, derr := common.FilterDistinctValues(iter, params.Key, cmp)
		if derr != nil {
			return nil, lazyerrors.Error(derr)
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"values", distinct,
				"ok", float64(1),
			)),
		)
	}

	c, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	params.Collation = h.effectiveCollation(connCtx, db, params.Collection, params.Collation)

	cmp := collation.Parse(params.Collation).Comparator()

	// With no filter, MongoDB answers distinct from an index led by the key
	// (DISTINCT_SCAN) and returns its index keys rather than document values.
	if params.Filter.Len() == 0 && cmp == nil {
		idx, err := distinctScanIndex(connCtx, c, params.Key)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		if idx != nil {
			distinct, err := distinctFromIndexKeys(connCtx, c, params.Key, idx)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"values", distinct,
					"ok", float64(1),
				)),
			)
		}
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	var qp backends.QueryParams
	qp.Collated = cmp != nil
	if fields, ok := common.FilterRootFields(params.Filter); ok {
		keyRoot, _, _ := strings.Cut(params.Key, ".")
		qp.Fields = append(fields, keyRoot)
	}
	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	queryRes, err := c.Query(connCtx, &qp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	closer.Add(queryRes.Iter)

	iter := common.FilterIteratorColl(queryRes.Iter, closer, params.Filter, cmp)

	distinct, err := common.FilterDistinctValues(iter, params.Key, cmp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"values", distinct,
			"ok", float64(1),
		)),
	)
}

// distinctScanIndex returns the index MongoDB would use for an unfiltered,
// uncollated distinct on key: one whose leading field is key, that is not
// partial, hidden, collated, or hashed/text/geo. It returns nil if none exists.
func distinctScanIndex(ctx context.Context, c backends.Collection, key string) (*backends.IndexInfo, error) {
	res, err := c.ListIndexes(ctx, &backends.ListIndexesParams{})
	if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	for i := range res.Indexes {
		idx := &res.Indexes[i]
		if len(idx.Key) == 0 || idx.Key[0].Field != key {
			continue
		}
		lead := idx.Key[0]
		if lead.Hashed || lead.Text || lead.Geo2D || lead.Geo2DSphere {
			continue
		}
		if idx.PartialFilterExpression != nil || idx.Hidden || !collation.Parse(idx.Collation).IsSimple() {
			continue
		}
		return idx, nil
	}

	return nil, nil
}

// distinctFromIndexKeys serves distinct from the backend's index scan when it
// can, otherwise from a full scan producing the same index keys.
func distinctFromIndexKeys(ctx context.Context, c backends.Collection, key string, idx *backends.IndexInfo) (*types.Array, error) {
	if ds, ok := c.(backends.DistinctScanner); ok {
		res, err := ds.DistinctScan(ctx, &backends.DistinctParams{Key: key})
		if err != nil {
			return nil, err
		}
		if res != nil {
			return common.DedupDistinctValues(res.Values)
		}
	}

	queryRes, err := c.Query(ctx, &backends.QueryParams{})
	if err != nil {
		return nil, err
	}

	return common.IndexKeyDistinctValues(queryRes.Iter, key, idx.Sparse)
}
