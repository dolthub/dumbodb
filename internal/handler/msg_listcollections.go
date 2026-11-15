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

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgListCollections implements `listCollections` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgListCollections(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document,
		"filter",
		"nameOnly",
		"authorizedCollections",
		"cursor",
	); err != nil {
		return nil, err
	}

	var filter *types.Document
	if filter, err = common.GetOptionalParam(document, "filter", filter); err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "comment")

	common.Ignored(document, h.L, "authorizedCollections")

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(dbName); err != nil {
		return nil, err
	}

	var nameOnly bool

	if v, _ := document.Get("nameOnly"); v != nil {
		if nameOnly, err = handlerparams.GetBoolOptionalParam("nameOnly", v); err != nil {
			return nil, err
		}
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s'", dbName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "listCollections")
		}

		return nil, lazyerrors.Error(err)
	}

	res, err := db.ListCollections(connCtx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	collections := types.MakeArray(len(res.Collections))

	for _, collection := range res.Collections {
		d, err := collectionCatalogEntry(&collection)
		if err != nil {
			return nil, err
		}

		matches, err := common.FilterDocument(d, filter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if !matches {
			continue
		}

		if nameOnly {
			d = must.NotFail(types.NewDocument(
				"name", collection.Name,
			))
		}

		collections.Append(d)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"id", int64(0),
				"ns", dbName+".$cmd.listCollections",
				"firstBatch", collections,
			)),
			"ok", float64(1),
		)),
	)
}

// collectionCatalogEntry returns the listCollections document of a collection
// or view, with its name, type, options, and info.
func collectionCatalogEntry(collection *backends.CollectionInfo) (*types.Document, error) {
	options := must.NotFail(types.NewDocument())
	info := must.NotFail(types.NewDocument("readOnly", false))

	collType := "collection"

	if collection.IsView {
		collType = "view"
		options.Set("viewOn", collection.ViewOn)
		if collection.ViewPipeline != nil {
			options.Set("pipeline", collection.ViewPipeline)
		} else {
			options.Set("pipeline", types.MakeArray(0))
		}
		if resolved := collation.Parse(collection.Collation).Resolve(); resolved != nil {
			options.Set("collation", resolved)
		}
		info.Set("readOnly", true)
	} else if collection.IsTimeSeries {
		collType = "timeseries"
		if resolved := collation.Parse(collection.Collation).Resolve(); resolved != nil {
			options.Set("collation", resolved)
		}
		options.Set("timeseries", timeSeriesOptions(collection))
	} else {
		options.Set("clusteredIndex", clusteredIndexSpec(collection.ClusteredIndexName))

		if collection.Capped() {
			options.Set("capped", true)
		}

		if collection.CappedSize > 0 {
			options.Set("size", collection.CappedSize)
		}

		if collection.CappedDocuments > 0 {
			options.Set("max", collection.CappedDocuments)
		}

		if collection.Validator != nil {
			options.Set("validator", collection.Validator)
		}

		// The default collation is reported resolved (server defaults plus the
		// ICU version), matching MongoDB.
		if collection.Collation != nil {
			if resolved := collation.Parse(collection.Collation).Resolve(); resolved != nil {
				options.Set("collation", resolved)
			}
		}

		if collection.ValidationLevel != "" {
			options.Set("validationLevel", collection.ValidationLevel)
		}

		if collection.ValidationAction != "" {
			options.Set("validationAction", collection.ValidationAction)
		}

		// mergeMode is a dumbodb option with no MongoDB counterpart. It is
		// reported here with the rest of a collection's configuration
		// rather than hidden, so a client can read back what it set.
		// Reported only when declared, matching how the validation
		// defaults are left unmaterialized.
		if collection.MergeMode != "" {
			options.Set("mergeMode", collection.MergeMode)
		}
	}

	// mongod reports no uuid or idIndex for a time-series view of its buckets.
	if collection.UUID != "" && !collection.IsTimeSeries {
		collUUID, err := uuid.Parse(collection.UUID)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		uuidBinary := types.Binary{
			Subtype: types.BinaryUUID,
			B:       must.NotFail(collUUID.MarshalBinary()),
		}

		info.Set("uuid", uuidBinary)
	}

	// Views and time-series collections have no _id index, and a clustered
	// collection reports its _id index as options.clusteredIndex instead.
	d := must.NotFail(types.NewDocument(
		"name", collection.Name,
		"type", collType,
		"options", options,
		"info", info,
	))

	return d, nil
}

// timeSeriesBucketMaxSpanSeconds is mongod's bucket span for each granularity.
var timeSeriesBucketMaxSpanSeconds = map[string]int32{
	"seconds": 3600,
	"minutes": 86400,
	"hours":   2592000,
}

// timeSeriesOptions returns a time-series collection's options as mongod
// reports them, with the default granularity and its bucket span filled in.
func timeSeriesOptions(collection *backends.CollectionInfo) *types.Document {
	opts := must.NotFail(types.NewDocument("timeField", collection.TimeField))
	if collection.MetaField != "" {
		opts.Set("metaField", collection.MetaField)
	}
	if collection.BucketSpanSeconds != 0 {
		opts.Set("bucketRoundingSeconds", collection.BucketSpanSeconds)
		opts.Set("bucketMaxSpanSeconds", collection.BucketSpanSeconds)
		return opts
	}
	granularity := collection.Granularity
	if granularity == "" {
		granularity = "seconds"
	}
	opts.Set("granularity", granularity)
	opts.Set("bucketMaxSpanSeconds", timeSeriesBucketMaxSpanSeconds[granularity])
	return opts
}
