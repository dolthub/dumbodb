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
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ExplainParams represents the parameters for the explain command.
type ExplainParams struct {
	DB         string `ferretdb:"$db"`
	Collection string `ferretdb:"collection"`

	Explain *types.Document `ferretdb:"explain"`

	Filter *types.Document `ferretdb:"filter,opt"`
	Sort   *types.Document `ferretdb:"sort,opt"`
	Skip   int64           `ferretdb:"skip,opt"`
	Limit  int64           `ferretdb:"limit,opt"`

	StagesDocs []any           `ferretdb:"-"`
	Aggregate  bool            `ferretdb:"-"`
	Command    *types.Document `ferretdb:"-"`

	// Verbosity controls what explain returns:
	// "queryPlanner" (default), "executionStats", or "allPlansExecution".
	Verbosity string

	ApiVersion           string `ferretdb:"apiVersion,ignored"`
	ApiStrict            bool   `ferretdb:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `ferretdb:"apiDeprecationErrors,ignored"`
}

// GetExplainParams returns the parameters for the explain command.
func GetExplainParams(document *types.Document, l *slog.Logger) (*ExplainParams, error) {
	var err error

	var db, collection string

	if db, err = GetRequiredParam[string](document, "$db"); err != nil {
		return nil, lazyerrors.Error(err)
	}

	var verbosity string
	if v, _ := document.Get("verbosity"); v != nil {
		if s, ok := v.(string); ok {
			verbosity = s
		}
	}
	if verbosity == "" {
		verbosity = "queryPlanner"
	}

	var cmd *types.Document

	cmd, err = GetRequiredParam[*types.Document](document, document.Command())
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if collectionVal, _ := cmd.Get(cmd.Command()); collectionVal != nil {
		if _, ok := collectionVal.(string); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"Failed to parse namespace element",
				document.Command(),
			)
		}
	}

	if collection, err = GetRequiredParam[string](cmd, cmd.Command()); err != nil {
		return nil, lazyerrors.Error(err)
	}

	var explain, filter, sort *types.Document

	cmd, err = GetRequiredParam[*types.Document](document, document.Command())
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	explain, err = GetRequiredParam[*types.Document](document, "explain")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	filter, err = GetOptionalParam(explain, "filter", filter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	sort, err = GetOptionalParam(explain, "sort", sort)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var limit, skip int64

	if limitVal, _ := explain.Get("limit"); limitVal != nil {
		var limitErr error
		if limit, limitErr = handlerparams.GetWholeNumberParam(limitVal); limitErr != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"BSON field 'FindCommandRequest.limit' is the wrong type '"+handlerparams.AliasFromType(limitVal)+"', expected types '[long, int, decimal, double']",
				"limit",
			)
		}
	}

	if limit, err = handlerparams.GetValidatedNumberParamWithMinValue("explain", "limit", limit, 0); err != nil {
		return nil, err
	}

	if skipVal, _ := explain.Get("skip"); skipVal != nil {
		var skipErr error
		if skip, skipErr = handlerparams.GetWholeNumberParam(skipVal); skipErr != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"BSON field 'FindCommandRequest.skip' is the wrong type '"+handlerparams.AliasFromType(skipVal)+"', expected types '[long, int, decimal, double']",
				"skip",
			)
		}
	}

	if skip, err = handlerparams.GetValidatedNumberParamWithMinValue("explain", "skip", skip, 0); err != nil {
		return nil, err
	}

	var stagesDocs []any

	if cmd.Command() == "aggregate" {
		var pipeline *types.Array

		pipeline, err = GetRequiredParam[*types.Array](explain, "pipeline")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrMissingField,
				"BSON field 'aggregate.pipeline' is missing but a required field",
				document.Command(),
			)
		}

		stagesDocs = must.NotFail(iterator.ConsumeValues(pipeline.Iterator()))
		for _, d := range stagesDocs {
			if _, ok := d.(*types.Document); !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					"Each element of the 'pipeline' array must be an object",
					document.Command(),
				)
			}
		}
	}

	return &ExplainParams{
		DB:         db,
		Collection: collection,
		Filter:     filter,
		Sort:       sort,
		Skip:       skip,
		Limit:      limit,
		StagesDocs: stagesDocs,
		Aggregate:  cmd.Command() == "aggregate",
		Command:    cmd,
		Verbosity:  verbosity,
	}, nil
}
