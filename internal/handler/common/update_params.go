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

	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/must"
)

// UpdateParams represents parameters for the update command.
//
//nolint:vet // for readability
type UpdateParams struct {
	DB         string `ferretdb:"$db"`
	Collection string `ferretdb:"update,collection"`

	Updates []Update `ferretdb:"updates"`

	Comment   string `ferretdb:"comment,opt"`
	MaxTimeMS int64  `ferretdb:"maxTimeMS,ignored"`

	Let *types.Document `ferretdb:"let,unimplemented"`

	Ordered                  bool            `ferretdb:"ordered,ignored"`
	BypassDocumentValidation bool            `ferretdb:"bypassDocumentValidation,ignored"`
	BypassEmptyTsReplacement bool            `ferretdb:"bypassEmptyTsReplacement,ignored"`
	WriteConcern             *types.Document `ferretdb:"writeConcern,ignored"`
	LSID                     any             `ferretdb:"lsid,ignored"`
	TxnNumber                int64           `ferretdb:"txnNumber,ignored"`
	Autocommit               bool            `ferretdb:"autocommit,ignored"`
	ClusterTime              any             `ferretdb:"$clusterTime,ignored"`
	ReadPreference           *types.Document `ferretdb:"$readPreference,ignored"`

	ApiVersion           string `ferretdb:"apiVersion,ignored"`
	ApiStrict            bool   `ferretdb:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `ferretdb:"apiDeprecationErrors,ignored"`
}

// Update represents a single update operation parameters.
//
//nolint:vet // for readability
type Update struct {
	Filter    *types.Document `ferretdb:"q,opt"`
	UpdateRaw any             `ferretdb:"u,opt"` // *types.Document or *types.Array (pipeline)
	Multi     bool            `ferretdb:"multi,opt"`
	Upsert    bool            `ferretdb:"upsert,opt,numericBool"`

	// Populated by GetUpdateParams from UpdateRaw.
	Update             *types.Document `ferretdb:"-"`
	Pipeline           *types.Array    `ferretdb:"-"`
	HasUpdateOperators bool            `ferretdb:"-"`
	IsPipeline         bool            `ferretdb:"-"`

	C            *types.Document `ferretdb:"c,unimplemented"`
	Collation    *types.Document `ferretdb:"collation,unimplemented"`
	ArrayFilters *types.Array    `ferretdb:"arrayFilters,opt"`

	Hint string `ferretdb:"hint,ignored"`
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
}

// GetUpdateParams returns parameters for update command.
func GetUpdateParams(document *types.Document, l *slog.Logger) (*UpdateParams, error) {
	var params UpdateParams

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
				if err != nil {
					return nil, err
				}

				if hasUpdateOperators {
					update.HasUpdateOperators = true

					if err := ValidateUpdateOperators(document.Command(), u); err != nil {
						return nil, err
					}
				} else if update.Multi {
					return nil, NewUpdateError(
						handlererrors.ErrFailedToParse,
						"multi update is not supported for replacement-style update",
						"update",
					)
				}

			case *types.Array:
				update.IsPipeline = true
				update.Pipeline = u

				// Validate each pipeline stage is a supported operator.
				for j := range u.Len() {
					stage, ok := must.NotFail(u.Get(j)).(*types.Document)
					if !ok {
						return nil, handlererrors.NewWriteErrorMsg(
							handlererrors.ErrFailedToParse,
							"A pipeline stage specification must be an object",
						)
					}

					if stage.Len() != 1 {
						return nil, handlererrors.NewWriteErrorMsg(
							handlererrors.ErrFailedToParse,
							"A pipeline stage specification object must contain exactly one field",
						)
					}

					stageName := stage.Keys()[0]
					switch stageName {
					case "$set", "$unset", "$addFields", "$project", "$replaceWith", "$replaceRoot":
						// supported pipeline stages for updates
					default:
						return nil, handlererrors.NewWriteErrorMsg(
							handlererrors.ErrNotImplemented,
							fmt.Sprintf("Unrecognized pipeline stage name: '%s'", stageName),
						)
					}
				}

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
