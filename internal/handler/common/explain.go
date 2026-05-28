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

type ExplainParams struct {
	DB         string `dumbo:"$db"`
	Collection string `dumbo:"collection"`

	Explain *types.Document `dumbo:"explain"`

	Filter     *types.Document `dumbo:"filter,opt"`
	Sort       *types.Document `dumbo:"sort,opt"`
	Projection *types.Document `dumbo:"projection,opt"`
	Skip       int64           `dumbo:"skip,opt"`
	Limit      int64           `dumbo:"limit,opt"`
	Hint       any             `dumbo:"hint,opt"`

	// CommandName is the inner sub-command being explained: "find", "count",
	// "distinct", or "aggregate".
	CommandName string `dumbo:"-"`

	// DistinctKey is set only when CommandName == "distinct".
	DistinctKey string `dumbo:"-"`

	StagesDocs []any           `dumbo:"-"`
	Aggregate  bool            `dumbo:"-"`
	Command    *types.Document `dumbo:"-"`

	// Verbosity controls what explain returns:
	// "queryPlanner" (default), "executionStats", or "allPlansExecution".
	Verbosity string

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

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

	commandName := cmd.Command()

	// find / aggregate use "filter"; count and distinct use "query".
	filter, err = GetOptionalParam(explain, "filter", filter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if filter == nil {
		filter, err = GetOptionalParam(explain, "query", filter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	sort, err = GetOptionalParam(explain, "sort", sort)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var projection *types.Document
	projection, err = GetOptionalParam(explain, "projection", projection)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var distinctKey string
	if commandName == "distinct" {
		if k, kerr := GetRequiredParam[string](explain, "key"); kerr == nil {
			distinctKey = k
		}
	}

	var hint any
	if hintVal, _ := explain.Get("hint"); hintVal != nil {
		hint = hintVal
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
		DB:          db,
		Collection:  collection,
		Filter:      filter,
		Sort:        sort,
		Projection:  projection,
		Skip:        skip,
		Limit:       limit,
		CommandName: commandName,
		DistinctKey: distinctKey,
		StagesDocs:  stagesDocs,
		Aggregate:   commandName == "aggregate",
		Command:     cmd,
		Verbosity:   verbosity,
		Hint:        hint,
	}, nil
}
