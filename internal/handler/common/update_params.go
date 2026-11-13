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
	"log/slog"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

//nolint:vet // for readability
type UpdateParams struct {
	DB         string `dumbo:"$db"`
	Collection string `dumbo:"update,collection"`

	Updates []Update `dumbo:"updates"`

	Comment   string `dumbo:"comment,opt"`
	MaxTimeMS int64  `dumbo:"maxTimeMS,opt,wholePositiveNumber"`

	Let *types.Document `dumbo:"let,unimplemented"`

	Ordered                  bool            `dumbo:"ordered,opt"`
	BypassDocumentValidation bool            `dumbo:"bypassDocumentValidation,opt"`
	BypassEmptyTsReplacement bool            `dumbo:"bypassEmptyTsReplacement,ignored"`
	WriteConcern             *types.Document `dumbo:"writeConcern,opt"`
	LSID                     any             `dumbo:"lsid,ignored"`
	TxnNumber                int64           `dumbo:"txnNumber,ignored"`
	StartTransaction         bool            `dumbo:"startTransaction,ignored"`
	Autocommit               bool            `dumbo:"autocommit,ignored"`
	ClusterTime              any             `dumbo:"$clusterTime,ignored"`
	ReadPreference           *types.Document `dumbo:"$readPreference,ignored"`
	ReadConcern              *types.Document `dumbo:"readConcern,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

//nolint:vet // for readability
type Update struct {
	Filter    *types.Document `dumbo:"q,opt"`
	UpdateRaw any             `dumbo:"u,opt"` // *types.Document or *types.Array (pipeline)
	Multi     bool            `dumbo:"multi,opt"`
	Upsert    bool            `dumbo:"upsert,opt,numericBool"`

	// Populated by GetUpdateParams from UpdateRaw.
	Update             *types.Document      `dumbo:"-"`
	Pipeline           *types.Array         `dumbo:"-"`
	PipelineStages     []aggregations.Stage `dumbo:"-"`
	HasUpdateOperators bool                 `dumbo:"-"`
	IsPipeline         bool                 `dumbo:"-"`
	// InvalidUpdate is the statement's update document error, which mongod
	// reports at the statement rather than failing the batch.
	InvalidUpdate error `dumbo:"-"`

	// Validator is nil when no active validator applies (absent, or
	// validationLevel "off"). ValidationLevel is "strict" or "moderate";
	// ValidationAction is "error" or "warn".
	Validator        *types.Document `dumbo:"-"`
	ValidationLevel  string          `dumbo:"-"`
	ValidationAction string          `dumbo:"-"`

	C            *types.Document `dumbo:"c,unimplemented"`
	Collation    *types.Document `dumbo:"collation,opt"`
	ArrayFilters *types.Array    `dumbo:"arrayFilters,opt"`

	Hint any `dumbo:"hint,opt"`
}

// UpdateResult is the result type returned from common.UpdateDocument.
// It represents the number of documents matched, modified and upserted.
// In case of findAndModify, it also contains pointers to the documents.
type UpdateResult struct {
	Upserted struct {
		Doc *types.Document
	}

	Matched struct {
		Doc   *types.Document
		Count int32
	}

	Modified struct {
		Doc   *types.Document
		Count int32
	}

	// WarnAllowed counts documents written despite failing the collection
	// validator because validationAction is "warn".
	WarnAllowed int32
}

func GetUpdateParams(document *types.Document, l *slog.Logger) (*UpdateParams, error) {
	params := UpdateParams{Ordered: true}

	err := handlerparams.ExtractParams(document, "update", &params, l)
	if err != nil {
		return nil, err
	}

	if len(params.Updates) > 0 {
		for i := range params.Updates {
			update := &params.Updates[i]

			if update.UpdateRaw == nil {
				continue
			}

			switch u := update.UpdateRaw.(type) {
			case *types.Document:
				update.Update = u

				hasUpdateOperators, err := HasSupportedUpdateModifiers("update", u)
				switch {
				case err != nil:
					update.InvalidUpdate = err
				case hasUpdateOperators:
					update.HasUpdateOperators = true

					update.InvalidUpdate = ValidateUpdateOperators(document.Command(), u)
				case update.Multi:
					update.InvalidUpdate = NewUpdateError(
						handlererrors.ErrFailedToParse,
						"multi update is not supported for replacement-style update",
						"update",
					)
				}

			case *types.Array:
				update.IsPipeline = true
				update.Pipeline = u

			default:
				return nil, handlererrors.NewWriteErrorMsg(
					handlererrors.ErrFailedToParse,
					"Update argument must be either an object or an array",
				)
			}
		}
	}

	return &params, nil
}
