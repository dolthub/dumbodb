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

package common

import (
	"fmt"
	"log/slog"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

//nolint:vet // for readability
type InsertParams struct {
	Docs       *types.Array `dumbo:"documents,opt"`
	DB         string       `dumbo:"$db"`
	Collection string       `dumbo:"insert,collection"`
	Ordered    bool         `dumbo:"ordered,opt"`

	MaxTimeMS                int64           `dumbo:"maxTimeMS,ignored"`
	WriteConcern             *types.Document `dumbo:"writeConcern,opt"`
	BypassDocumentValidation bool            `dumbo:"bypassDocumentValidation,opt"`
	BypassEmptyTsReplacement bool            `dumbo:"bypassEmptyTsReplacement,ignored"`
	Comment                  string          `dumbo:"comment,ignored"`
	LSID                     any             `dumbo:"lsid,ignored"`
	TxnNumber                int64           `dumbo:"txnNumber,ignored"`
	StartTransaction         bool            `dumbo:"startTransaction,ignored"`
	Autocommit               bool            `dumbo:"autocommit,ignored"`
	ClusterTime              any             `dumbo:"$clusterTime,ignored"`
	ReadPreference           *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`

	// SkipDurableSync is derived from WriteConcern in GetInsertParams and
	// propagated into backend params so the storage layer can skip the
	// synchronous NBS journal fsync. Not populated from the wire.
	SkipDurableSync bool `dumbo:"-"`
}

func GetInsertParams(document *types.Document, l *slog.Logger) (*InsertParams, error) {
	params := InsertParams{
		Ordered: true,
	}

	err := handlerparams.ExtractParams(document, "insert", &params, l)
	if err != nil {
		return nil, err
	}

	params.SkipDurableSync = DecideWriteConcern(params.WriteConcern).SkipDurableSync

	for i := 0; i < params.Docs.Len(); i++ {
		doc := must.NotFail(params.Docs.Get(i))

		if _, ok := doc.(*types.Document); !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf(
					"BSON field 'insert.documents.%d' is the wrong type '%s', expected type 'object'",
					i,
					handlerparams.AliasFromType(doc),
				),
			)
		}
	}

	return &params, nil
}
