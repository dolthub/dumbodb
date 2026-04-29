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

	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// DeleteParams represents parameters for the delete command.
//
//nolint:vet // for readability
type DeleteParams struct {
	DB         string `dumbo:"$db"`
	Collection string `dumbo:"delete,collection"`

	Deletes []Delete `dumbo:"deletes,opt"`
	Comment string   `dumbo:"comment,opt"`
	Ordered bool     `dumbo:"ordered,opt"`

	Let *types.Document `dumbo:"let,unimplemented"`

	MaxTimeMS      int64           `dumbo:"maxTimeMS,ignored"`
	WriteConcern   *types.Document `dumbo:"writeConcern,opt"`
	LSID           any             `dumbo:"lsid,ignored"`
	TxnNumber      int64           `dumbo:"txnNumber,ignored"`
	ClusterTime    any             `dumbo:"$clusterTime,ignored"`
	ReadPreference *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`

	// SkipDurableSync is derived from WriteConcern in GetDeleteParams and
	// propagated into backend params so the storage layer can skip the
	// synchronous NBS journal fsync. Not populated from the wire.
	SkipDurableSync bool `dumbo:"-"`
}

// Delete represents single delete operation parameters.
//
//nolint:vet // for readability
type Delete struct {
	Filter  *types.Document `dumbo:"q"`
	Limited bool            `dumbo:"limit,zeroOrOneAsDeleteLimit"`

	Collation *types.Document `dumbo:"collation,unimplemented"`

	Hint string `dumbo:"hint,ignored"`
}

// GetDeleteParams returns parameters for delete operation.
func GetDeleteParams(document *types.Document, l *slog.Logger) (*DeleteParams, error) {
	params := DeleteParams{
		Ordered: true,
	}

	err := handlerparams.ExtractParams(document, "delete", &params, l)
	if err != nil {
		return nil, err
	}

	params.SkipDurableSync = DecideWriteConcern(params.WriteConcern).SkipDurableSync

	return &params, nil
}
