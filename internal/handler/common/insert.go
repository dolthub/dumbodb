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

// InsertParams represents the parameters for an insert command.
//
//nolint:vet // for readability
type InsertParams struct {
	Docs       *types.Array `ferretdb:"documents,opt"`
	DB         string       `ferretdb:"$db"`
	Collection string       `ferretdb:"insert,collection"`
	Ordered    bool         `ferretdb:"ordered,opt"`

	MaxTimeMS                int64           `ferretdb:"maxTimeMS,ignored"`
	WriteConcern             *types.Document `ferretdb:"writeConcern,opt"`
	BypassDocumentValidation bool            `ferretdb:"bypassDocumentValidation,ignored"`
	BypassEmptyTsReplacement bool            `ferretdb:"bypassEmptyTsReplacement,ignored"`
	Comment                  string          `ferretdb:"comment,ignored"`
	LSID                     any             `ferretdb:"lsid,ignored"`
	TxnNumber                int64           `ferretdb:"txnNumber,ignored"`
	StartTransaction         bool            `ferretdb:"startTransaction,ignored"`
	Autocommit               bool            `ferretdb:"autocommit,ignored"`
	ClusterTime              any             `ferretdb:"$clusterTime,ignored"`
	ReadPreference           *types.Document `ferretdb:"$readPreference,ignored"`

	ApiVersion           string `ferretdb:"apiVersion,ignored"`
	ApiStrict            bool   `ferretdb:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `ferretdb:"apiDeprecationErrors,ignored"`

	// SkipDurableSync is derived from WriteConcern in GetInsertParams and
	// propagated into backend params so the storage layer can skip the
	// synchronous NBS journal fsync. Not populated from the wire.
	SkipDurableSync bool `ferretdb:"-"`
}

// GetInsertParams returns the parameters for an insert command.
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
