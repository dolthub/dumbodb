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

// UpdateParams represents parameters for the update command.
//
//nolint:vet // for readability
type UpdateParams struct {
	DB         string `dumbo:"$db"`
	Collection string `dumbo:"update,collection"`

	Updates []Update `dumbo:"updates"`

	Comment   string `dumbo:"comment,opt"`
	MaxTimeMS int64  `dumbo:"maxTimeMS,ignored"`

	Let *types.Document `dumbo:"let,unimplemented"`

	Ordered                  bool            `dumbo:"ordered,ignored"`
	BypassDocumentValidation bool            `dumbo:"bypassDocumentValidation,ignored"`
	BypassEmptyTsReplacement bool            `dumbo:"bypassEmptyTsReplacement,ignored"`
	WriteConcern             *types.Document `dumbo:"writeConcern,opt"`
	LSID                     any             `dumbo:"lsid,ignored"`
	TxnNumber                int64           `dumbo:"txnNumber,ignored"`
	Autocommit               bool            `dumbo:"autocommit,ignored"`
	ClusterTime              any             `dumbo:"$clusterTime,ignored"`
	ReadPreference           *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`

	// SkipDurableSync is derived from WriteConcern in GetUpdateParams and
	// propagated into backend params so the storage layer can skip the
	// synchronous NBS journal fsync. Not populated from the wire.
	SkipDurableSync bool `dumbo:"-"`
}

// Update represents a single update operation parameters.
//
//nolint:vet // for readability
type Update struct {
	Filter    *types.Document `dumbo:"q,opt"`
	UpdateRaw any             `dumbo:"u,opt"` // *types.Document or *types.Array (pipeline)
	Multi     bool            `dumbo:"multi,opt"`
	Upsert    bool            `dumbo:"upsert,opt,numericBool"`

	// Populated by GetUpdateParams from UpdateRaw.
	Update             *types.Document `dumbo:"-"`
	Pipeline           *types.Array    `dumbo:"-"`
	HasUpdateOperators bool            `dumbo:"-"`
	IsPipeline         bool            `dumbo:"-"`

	C            *types.Document `dumbo:"c,unimplemented"`
	Collation    *types.Document `dumbo:"collation,unimplemented"`
	ArrayFilters *types.Array    `dumbo:"arrayFilters,opt"`

	Hint string `dumbo:"hint,ignored"`
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

	params.SkipDurableSync = DecideWriteConcern(params.WriteConcern).SkipDurableSync

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
