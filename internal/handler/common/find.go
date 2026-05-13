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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

//nolint:vet // for readability
type FindParams struct {
	DB           string          `dumbo:"$db"`
	Collection   string          `dumbo:"find,collection"`
	Filter       *types.Document `dumbo:"filter,opt"`
	Sort         *types.Document `dumbo:"sort,opt"`
	Projection   *types.Document `dumbo:"projection,opt"`
	Skip         int64           `dumbo:"skip,opt,positiveNumber"`
	Limit        int64           `dumbo:"limit,opt,positiveNumber"`
	BatchSize    int64           `dumbo:"batchSize,opt,positiveNumber"`
	SingleBatch  bool            `dumbo:"singleBatch,opt"`
	Comment      string          `dumbo:"comment,opt"`
	MaxTimeMS    int64           `dumbo:"maxTimeMS,opt,wholePositiveNumber"`
	ShowRecordId bool            `dumbo:"showRecordId,opt"`
	Tailable     bool            `dumbo:"tailable,opt"`
	AwaitData    bool            `dumbo:"awaitData,opt"`

	Collation *types.Document `dumbo:"collation,opt"`
	Let       *types.Document `dumbo:"let,unimplemented"`

	// ParsedCollation is derived from Collation after ExtractParams.
	ParsedCollation *Collation `dumbo:"-"`

	AllowDiskUse     bool            `dumbo:"allowDiskUse,ignored"`
	ReadConcern      *types.Document `dumbo:"readConcern,ignored"`
	Max              *types.Document `dumbo:"max,opt"`
	Min              *types.Document `dumbo:"min,opt"`
	Hint             any             `dumbo:"hint,opt"`
	LSID             any             `dumbo:"lsid,ignored"`
	TxnNumber        int64           `dumbo:"txnNumber,ignored"`
	StartTransaction bool            `dumbo:"startTransaction,ignored"`
	Autocommit       bool            `dumbo:"autocommit,ignored"`
	ClusterTime      any             `dumbo:"$clusterTime,ignored"`
	ReadPreference   *types.Document `dumbo:"$readPreference,ignored"`

	ReturnKey           bool `dumbo:"returnKey,opt"`
	OplogReplay         bool `dumbo:"oplogReplay,ignored"`
	AllowPartialResults bool `dumbo:"allowPartialResults,ignored"`

	NoCursorTimeout bool `dumbo:"noCursorTimeout,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

func GetFindParams(doc *types.Document, l *slog.Logger) (*FindParams, error) {
	params := FindParams{
		BatchSize: 101,
	}

	// Pre-validate sort type: MongoDB returns a specific "Expected field sortto be of type object"
	// message (with the MongoDB typo "sortto") when sort is not a document.
	if sortVal, err := doc.Get("sort"); err == nil {
		if _, ok := sortVal.(*types.Document); !ok {
			if _, isNull := sortVal.(types.NullType); !isNull {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					"Expected field sortto be of type object",
					"find",
				)
			}
		}
	}

	// Pre-validate showRecordId type: MongoDB returns "Field 'showRecordId' should be a boolean value"
	// instead of the generic BSON type mismatch message.
	if showRecordIdVal, err := doc.Get("showRecordId"); err == nil {
		if _, ok := showRecordIdVal.(bool); !ok {
			typeName := handlerparams.AliasFromType(showRecordIdVal)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Field 'showRecordId' should be a boolean value, but found: "+typeName,
				"find",
			)
		}
	}

	if err := handlerparams.ExtractParams(doc, "find", &params, l); err != nil {
		return nil, err
	}

	params.ParsedCollation = ParseCollation(params.Collation)

	if params.AwaitData && !params.Tailable {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"Cannot set 'awaitData' without also setting 'tailable'",
			"find",
		)
	}

	return &params, nil
}
