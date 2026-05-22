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
)

//nolint:vet // for readability
type FindAndModifyParams struct {
	DB                string          `dumbo:"$db"`
	Collection        string          `dumbo:"findAndModify,collection"`
	Comment           string          `dumbo:"comment,opt"`
	Query             *types.Document `dumbo:"query,opt"`
	Sort              *types.Document `dumbo:"sort,opt"`
	UpdateValue       any             `dumbo:"update,opt"`
	Remove            bool            `dumbo:"remove,opt"`
	Upsert            bool            `dumbo:"upsert,opt"`
	ReturnNewDocument bool            `dumbo:"new,opt,numericBool"`
	MaxTimeMS         int64           `dumbo:"maxTimeMS,opt,wholePositiveNumber"`

	Update      *types.Document `dumbo:"-"`
	Aggregation *types.Array    `dumbo:"-"`

	HasUpdateOperators bool `dumbo:"-"`

	Fields *types.Document `dumbo:"fields,opt"`

	Let          *types.Document `dumbo:"let,unimplemented"`
	Collation    *types.Document `dumbo:"collation,unimplemented"`
	ArrayFilters *types.Array    `dumbo:"arrayFilters,opt"`

	Hint                     string          `dumbo:"hint,ignored"`
	WriteConcern             *types.Document `dumbo:"writeConcern,opt"`
	BypassDocumentValidation bool            `dumbo:"bypassDocumentValidation,ignored"`
	BypassEmptyTsReplacement bool            `dumbo:"bypassEmptyTsReplacement,ignored"`
	LSID                     any             `dumbo:"lsid,ignored"`
	TxnNumber                int64           `dumbo:"txnNumber,ignored"`
	StartTransaction         bool            `dumbo:"startTransaction,ignored"`
	Autocommit               bool            `dumbo:"autocommit,ignored"`
	ClusterTime              any             `dumbo:"$clusterTime,ignored"`
	ReadPreference           *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`

	// SkipDurableSync is derived from WriteConcern in GetFindAndModifyParams and
	// propagated into backend params so the storage layer can skip the
	// synchronous NBS journal fsync. Not populated from the wire.
	SkipDurableSync bool `dumbo:"-"`
}

func GetFindAndModifyParams(doc *types.Document, l *slog.Logger) (*FindAndModifyParams, error) {
	var params FindAndModifyParams

	err := handlerparams.ExtractParams(doc, "findAndModify", &params, l)
	if err != nil {
		return nil, err
	}

	params.SkipDurableSync = DecideWriteConcern(params.WriteConcern).SkipDurableSync

	if params.Collection == "" {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrInvalidNamespace,
			fmt.Sprintf("Invalid namespace specified '%s.'", params.DB),
		)
	}

	if params.UpdateValue == nil && !params.Remove {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrFailedToParse,
			"Either an update or remove=true must be specified",
		)
	}

	if params.ReturnNewDocument && params.Remove {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrFailedToParse,
			"Cannot specify both new=true and remove=true; 'remove' always returns the deleted document",
		)
	}

	if params.UpdateValue != nil {
		switch updateParam := params.UpdateValue.(type) {
		case *types.Document:
			params.Update = updateParam
		case *types.Array:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"Aggregation pipelines are not supported yet",
				"update",
			)
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"Update argument must be either an object or an array",
				"update",
			)
		}
	}

	if params.Update != nil && params.Remove {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrFailedToParse,
			"Cannot specify both an update and remove=true",
		)
	}

	if params.Upsert && params.Remove {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrFailedToParse,
			"Cannot specify both upsert=true and remove=true",
		)
	}

	hasUpdateOperators, err := HasSupportedUpdateModifiers("findAndModify", params.Update)
	if err != nil {
		return nil, err
	}

	params.HasUpdateOperators = hasUpdateOperators

	return &params, nil
}
