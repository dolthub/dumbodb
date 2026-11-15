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
	"strings"

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
			switch from := from.(type) {
			case string:
				if err := checkLiteNamespace(name, from); err != nil {
					return err
				}
			case *types.Document:
			default:
				return fail(handlererrors.ErrFailedToParse,
					"$lookup 'from' field must be a string, but found "+handlerparams.AliasFromType(from))
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
					// mongod parses the fields in order.
					for _, key := range doc.Keys() {
						if key == "into" {
							break
						}
						if !mergeSpecFields[key] {
							return fail(handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field '$merge.%s' is an unknown field.", key))
						}
					}
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
	case "$collStats":
		return CheckCollStatsSpec(stage)
	case "$listLocalSessions", "$listSessions":
		return CheckListSessionsSpec(stage)
	case "$listCachedAndActiveUsers":
		_, err := newListCachedAndActiveUsers(stage)
		return err
	case "$graphLookup":
		_, err := graphLookupFrom(stage)
		return err
	}
	return nil
}

// graphLookupFrom returns the collection named by the 'from' field of a
// $graphLookup stage, or mongod's lite parse error for the field.
func graphLookupFrom(stage *types.Document) (string, error) {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$graphLookup (stage)")
	}
	spec := must.NotFail(stage.Get("$graphLookup"))
	specDoc, ok := spec.(*types.Document)
	if !ok {
		return "", fail(handlererrors.ErrFailedToParse,
			"the $graphLookup stage specification must be an object, but found "+handlerparams.AliasFromType(spec))
	}
	fromVal, err := specDoc.Get("from")
	if err != nil {
		return "", fail(handlererrors.ErrFailedToParse,
			"missing 'from' option to $graphLookup stage specification: "+types.FormatAnyValue(specDoc))
	}
	switch from := fromVal.(type) {
	case string:
		return from, checkLiteNamespace("$graphLookup", from)
	case *types.Document:
		var db, coll string
		for _, key := range from.Keys() {
			v := must.NotFail(from.Get(key))
			if key != "db" && key != "coll" {
				return "", fail(handlererrors.ErrIDLUnknownField, "BSON field 'from."+key+"' is an unknown field.")
			}
			s, isString := v.(string)
			if !isString {
				return "", fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'from.%s' is the wrong type '%s', expected type 'string'", key, handlerparams.AliasFromType(v)))
			}
			if key == "db" {
				db = s
			} else {
				coll = s
			}
		}
		return "", fail(handlererrors.ErrFailedToParse, fmt.Sprintf(
			"$graphLookup with syntax {from: {db:<>, coll:<>},..} is not supported for db: %s and coll: %s", db, coll))
	default:
		return "", fail(handlererrors.ErrFailedToParse,
			"$graphLookup 'from' field must be a string, but found "+handlerparams.AliasFromType(fromVal))
	}
}

// checkLiteNamespace returns mongod's error for a collection name that does
// not form a valid namespace.
func checkLiteNamespace(stageName, coll string) error {
	var msg string
	switch {
	case strings.HasPrefix(coll, "."):
		msg = "Collection names cannot start with '.': " + coll
	case strings.ContainsRune(coll, 0):
		msg = "namespaces cannot have embedded null characters"
	case coll == "":
		msg = "invalid " + stageName + " namespace"
	default:
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, stageName+" (stage)")
}

// CheckListSessionsSpec returns the error mongod's IDL parser gives for a
// $listLocalSessions or $listSessions specification.
func CheckListSessionsSpec(stage *types.Document) error {
	name := stage.Command()
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, name+" (stage)")
	}
	wrongType := func(field string, v any, want string) error {
		return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf("BSON field '%s.%s' is the wrong type '%s', expected type '%s'",
			name, field, handlerparams.AliasFromType(v), want))
	}
	spec, ok := must.NotFail(stage.Get(name)).(*types.Document)
	if !ok {
		return fail(handlererrors.ErrTypeMismatch, name+" options must be specified in an object, but found: "+
			handlerparams.AliasFromType(must.NotFail(stage.Get(name))))
	}
	for _, key := range spec.Keys() {
		v := must.NotFail(spec.Get(key))
		switch key {
		case "$_internalPredicate":
			if _, isDoc := v.(*types.Document); !isDoc {
				return wrongType(key, v, "object")
			}
		case "allUsers":
			if _, isBool := v.(bool); !isBool {
				return wrongType(key, v, "bool")
			}
		case "users":
			users, isArray := v.(*types.Array)
			if !isArray {
				return wrongType(key, v, "array")
			}
			for i := range users.Len() {
				u := must.NotFail(users.Get(i))
				user, isDoc := u.(*types.Document)
				if !isDoc {
					return wrongType(fmt.Sprintf("users.%d", i), u, "object")
				}
				for _, k := range user.Keys() {
					f := must.NotFail(user.Get(k))
					switch k {
					case "user", "db":
						if _, isStr := f.(string); !isStr {
							return wrongType("users."+k, f, "string")
						}
					default:
						return fail(handlererrors.ErrIDLUnknownField, "BSON field '"+name+".users."+k+"' is an unknown field.")
					}
				}
				for _, k := range []string{"user", "db"} {
					if !user.Has(k) {
						return fail(handlererrors.ErrMissingField,
							"BSON field '"+name+".users."+k+"' is missing but a required field")
					}
				}
			}
		default:
			return fail(handlererrors.ErrIDLUnknownField, "BSON field '"+name+"."+key+"' is an unknown field.")
		}
	}
	return nil
}

// mergeSpecFields are the fields of a $merge specification document.
var mergeSpecFields = map[string]bool{
	"into": true, "on": true, "let": true, "whenMatched": true, "whenNotMatched": true,
}
