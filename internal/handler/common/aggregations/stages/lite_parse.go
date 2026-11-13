// Copyright 2026 Dolthub, Inc.
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

package stages

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// LiteParsePipeline is mongod's first pass over a pipeline, before any stage
// is built: every stage name is checked, in $lookup, $unionWith and $facet
// sub-pipelines too, and only the stages with a lite parser have their
// specification checked, so an unknown stage fails before an earlier stage's
// own errors.
func LiteParsePipeline(pipeline *types.Array) error {
	return liteParse(pipeline, "Each element of the 'pipeline' array must be an object")
}

func liteParse(pipeline *types.Array, notObject string) error {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch, notObject, "aggregate")
		}
		if stage.Len() != 1 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageInvalid,
				"A pipeline stage specification object must contain exactly one field.", "aggregate")
		}
		name := stage.Command()
		_, known := stageConstructors[name]
		_, failing := parseFailingStages[name]
		if !known && !failing && !specialStages[name] {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageUnrecognized,
				fmt.Sprintf("Unrecognized pipeline stage name: '%s'", name), name+" (stage)")
		}
		if err := liteParseStage(stage); err != nil {
			return err
		}
	}
	return nil
}

// liteParseStage checks the specification of a stage with a lite parser.
func liteParseStage(stage *types.Document) error {
	name := stage.Command()
	spec := must.NotFail(stage.Get(name))
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, name+" (stage)")
	}
	subPipeline := func(v any) error {
		p, ok := v.(*types.Array)
		if !ok {
			return fail(handlererrors.ErrTypeMismatch, "A pipeline must be an array of objects")
		}
		return liteParse(p, "A pipeline must be an array of objects")
	}
	doc, isDoc := spec.(*types.Document)
	_, isString := spec.(string)

	switch name {
	case "$facet":
		if !isDoc || doc.Len() == 0 {
			return fail(handlererrors.ErrFacetSpecNotObject,
				"the $facet specification must be a non-empty object, but found: $facet: "+types.FormatAnyValue(spec))
		}
		for _, k := range doc.Keys() {
			p, ok := must.NotFail(doc.Get(k)).(*types.Array)
			if !ok {
				return fail(handlererrors.ErrFacetArgumentNotArray,
					fmt.Sprintf("arguments to $facet must be arrays, %s is type %s", k, handlerparams.AliasFromType(must.NotFail(doc.Get(k)))))
			}
			if err := liteParse(p, "A pipeline must be an array of objects"); err != nil {
				return err
			}
		}
	case "$lookup":
		if !isDoc {
			return fail(handlererrors.ErrFailedToParse,
				"the $lookup stage specification must be an object, but found "+handlerparams.AliasFromType(spec))
		}
		if from, err := doc.Get("from"); err == nil {
			if _, ok := from.(string); !ok {
				if _, ok := from.(*types.Document); !ok {
					return fail(handlererrors.ErrFailedToParse,
						"$lookup 'from' field must be a string, but found "+handlerparams.AliasFromType(from))
				}
			}
		}
		if p, err := doc.Get("pipeline"); err == nil {
			return subPipeline(p)
		}
	case "$unionWith":
		if isString {
			return nil
		}
		if !isDoc {
			return fail(handlererrors.ErrFailedToParse,
				"the $unionWith stage specification must be an object or string, but found "+handlerparams.AliasFromType(spec))
		}
		if coll, err := doc.Get("coll"); err == nil {
			if _, ok := coll.(string); !ok {
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field '$unionWith.coll' is the wrong type '%s', expected type 'string'", handlerparams.AliasFromType(coll)))
			}
		}
		if p, err := doc.Get("pipeline"); err == nil {
			return subPipeline(p)
		}
	case "$out":
		switch {
		case isString:
		case isDoc:
			for _, field := range []string{"db", "coll"} {
				if v, err := doc.Get(field); err == nil {
					if _, ok := v.(string); !ok {
						return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
							"BSON field '$out.%s' is the wrong type '%s', expected type 'string'", field, handlerparams.AliasFromType(v)))
					}
				}
			}
		default:
			return fail(handlererrors.ErrOutSpecType,
				"$out only supports a string or object argument, but found "+handlerparams.AliasFromType(spec))
		}
	case "$merge":
		switch {
		case isString:
		case isDoc:
			if into, err := doc.Get("into"); err == nil {
				_, s := into.(string)
				_, d := into.(*types.Document)
				if !s && !d {
					return fail(handlererrors.ErrMergeIntoType,
						"$merge 'into' field  must be either a string or an object, but found "+handlerparams.AliasFromType(into))
				}
			}
		default:
			return fail(handlererrors.ErrTypeMismatch,
				"$merge requires a string or object argument, but found "+handlerparams.AliasFromType(spec))
		}
	case "$changeStream":
		if !isDoc {
			return fail(handlererrors.ErrChangeStreamSpecType,
				"$changeStream must take a nested object but found: $changeStream: "+types.FormatAnyValue(spec))
		}
	case "$queryStats":
		return CheckQueryStatsSpec(stage)
	case "$querySettings":
		return CheckQuerySettingsSpec(stage)
	case "$currentOp":
		return CheckCurrentOpSpec(stage)
	}
	return nil
}
