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

	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
)

// FindParams represents parameters for the find command.
//
//nolint:vet // for readability
type FindParams struct {
	DB           string          `ferretdb:"$db"`
	Collection   string          `ferretdb:"find,collection"`
	Filter       *types.Document `ferretdb:"filter,opt"`
	Sort         *types.Document `ferretdb:"sort,opt"`
	Projection   *types.Document `ferretdb:"projection,opt"`
	Skip         int64           `ferretdb:"skip,opt,positiveNumber"`
	Limit        int64           `ferretdb:"limit,opt,positiveNumber"`
	BatchSize    int64           `ferretdb:"batchSize,opt,positiveNumber"`
	SingleBatch  bool            `ferretdb:"singleBatch,opt"`
	Comment      string          `ferretdb:"comment,opt"`
	MaxTimeMS    int64           `ferretdb:"maxTimeMS,opt,wholePositiveNumber"`
	ShowRecordId bool            `ferretdb:"showRecordId,opt"`
	Tailable     bool            `ferretdb:"tailable,opt"`
	AwaitData    bool            `ferretdb:"awaitData,opt"`

	Collation *types.Document `ferretdb:"collation,ignored"`
	Let       *types.Document `ferretdb:"let,unimplemented"`

	AllowDiskUse     bool            `ferretdb:"allowDiskUse,ignored"`
	ReadConcern      *types.Document `ferretdb:"readConcern,ignored"`
	Max              *types.Document `ferretdb:"max,opt"`
	Min              *types.Document `ferretdb:"min,opt"`
	Hint             any             `ferretdb:"hint,opt"`
	LSID             any             `ferretdb:"lsid,ignored"`
	TxnNumber        int64           `ferretdb:"txnNumber,ignored"`
	StartTransaction bool            `ferretdb:"startTransaction,ignored"`
	Autocommit       bool            `ferretdb:"autocommit,ignored"`
	ClusterTime      any             `ferretdb:"$clusterTime,ignored"`
	ReadPreference   *types.Document `ferretdb:"$readPreference,ignored"`

	ReturnKey           bool `ferretdb:"returnKey,opt"`
	OplogReplay         bool `ferretdb:"oplogReplay,ignored"`
	AllowPartialResults bool `ferretdb:"allowPartialResults,ignored"`

	NoCursorTimeout bool `ferretdb:"noCursorTimeout,ignored"`

	ApiVersion           string `ferretdb:"apiVersion,ignored"`
	ApiStrict            bool   `ferretdb:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `ferretdb:"apiDeprecationErrors,ignored"`
}

// GetFindParams returns `find` command parameters.
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

	if params.AwaitData && !params.Tailable {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"Cannot set 'awaitData' without also setting 'tailable'",
			"find",
		)
	}

	return &params, nil
}
