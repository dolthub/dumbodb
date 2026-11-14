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
	"context"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// fill represents the $fill stage, which mongod rewrites as a
// $setWindowFields of $locf and $linearFill for 'method' outputs followed by
// an $addFields of $ifNull for 'value' outputs.
type fill struct {
	stages []aggregations.Stage
}

func newFill(stage *types.Document) (aggregations.Stage, error) {
	v := must.NotFail(stage.Get("$fill"))
	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"The $fill stage specification must be an object, found "+handlerparams.AliasFromType(v), "$fill (stage)")
	}

	var output, sortBy, partitionBy, partitionByFields any
	for _, key := range spec.Keys() {
		val := must.NotFail(spec.Get(key))
		if _, isNull := val.(types.NullType); isNull && key != "partitionBy" {
			switch key {
			case "output", "sortBy", "partitionByFields":
				// mongod's IDL parser treats a null field as absent.
				continue
			}
		}
		switch key {
		case "output":
			output = val
		case "sortBy":
			sortBy = val
		case "partitionBy":
			// mongod's IDL parser checks the type of each field as it reads it.
			switch val.(type) {
			case types.NullType, types.UndefinedType:
				return nil, fillWrongType("partitionBy", val, "object")
			case string, *types.Document:
			default:
				// mongod's message prints its list of allowed types this way.
				return nil, fillError(handlererrors.ErrTypeMismatch, "BSON field '$fill.partitionBy' is the wrong type '"+
					handlerparams.AliasFromType(val)+"', expected types '[']")
			}
			partitionBy = val
		case "partitionByFields":
			partitionByFields = val
		default:
			return nil, fillError(handlererrors.ErrIDLUnknownField, "BSON field '$fill."+key+"' is an unknown field.")
		}
	}
	if output == nil {
		return nil, fillError(handlererrors.ErrMissingField, "BSON field '$fill.output' is missing but a required field")
	}
	outputDoc, ok := output.(*types.Document)
	if !ok {
		return nil, fillWrongType("output", output, "object")
	}
	var sortDoc *types.Document
	if sortBy != nil {
		if sortDoc, ok = sortBy.(*types.Document); !ok {
			return nil, fillWrongType("sortBy", sortBy, "object")
		}
	}

	if partitionByFields != nil {
		arr, ok := partitionByFields.(*types.Array)
		if !ok {
			return nil, fillWrongType("partitionByFields", partitionByFields, "array")
		}
		if partitionBy != nil {
			return nil, fillError(handlererrors.ErrFillBothPartitions,
				"Maximum one of 'partitionBy' and 'partitionByFields can be specified in '$fill'")
		}
		key := types.MakeDocument(arr.Len())
		for i := range arr.Len() {
			path, err := stagePath(must.NotFail(arr.Get(i)), fmt.Sprintf("$fill.partitionByFields.%d", i), "$fill (stage)")
			if err != nil {
				return nil, err
			}
			key.Set(fmt.Sprintf("p%d", i), "$"+path.String())
		}
		partitionBy = key
	}

	if outputDoc.Len() == 0 {
		return nil, fillError(handlererrors.ErrFillEmptyOutput,
			"The output field in '$fill' must contain an object with at least one element")
	}

	windowOutput := types.MakeDocument(0)
	values := types.MakeDocument(0)
	for _, field := range outputDoc.Keys() {
		fieldSpec, ok := must.NotFail(outputDoc.Get(field)).(*types.Document)
		if !ok {
			return nil, fillError(handlererrors.ErrFillOutputSpec,
				"Each fill output specification must be an object with exactly one field")
		}
		var method, value any
		for _, key := range fieldSpec.Keys() {
			switch v := must.NotFail(fieldSpec.Get(key)); key {
			case "method":
				// mongod's IDL parser checks the type as it reads the field.
				if _, isNull := v.(types.NullType); !isNull {
					if _, ok := v.(string); !ok {
						return nil, fillWrongType("method", v, "string")
					}
					method = v
				}
			case "value":
				value = v
			default:
				return nil, fillError(handlererrors.ErrIDLUnknownField, "BSON field '$fill."+key+"' is an unknown field.")
			}
		}
		switch {
		case method != nil && value != nil:
			return nil, fillError(handlererrors.ErrFillMethodAndValue,
				"Each fill output specification must have exactly one of 'method' or 'value' fields, not both")
		case value != nil:
			values.Set(field, must.NotFail(types.NewDocument("$ifNull", must.NotFail(types.NewArray("$"+field, value)))))
		case method != nil:
			var op string
			switch method {
			case "locf":
				op = "$locf"
			case "linear":
				op = "$linearFill"
				if sortDoc == nil || sortDoc.Len() != 1 {
					return nil, fillError(handlererrors.ErrLinearFillSortBy,
						"$linearFill must be specified with a top level sortBy expression with exactly one element")
				}
			default:
				return nil, fillError(handlererrors.ErrFillMethod, "Method must be either locf or linear")
			}
			windowOutput.Set(field, must.NotFail(types.NewDocument(op, "$"+field)))
		}
	}

	f := new(fill)
	if windowOutput.Len() > 0 {
		windowSpec := types.MakeDocument(3)
		if partitionBy != nil {
			windowSpec.Set("partitionBy", partitionBy)
		}
		if sortDoc != nil {
			windowSpec.Set("sortBy", sortDoc)
		}
		windowSpec.Set("output", windowOutput)
		s, err := newSetWindowFields(must.NotFail(types.NewDocument("$setWindowFields", windowSpec)))
		if err != nil {
			return nil, err
		}
		f.stages = append(f.stages, s)
	}
	if values.Len() > 0 {
		s, err := newAddFields(must.NotFail(types.NewDocument("$addFields", values)))
		if err != nil {
			return nil, err
		}
		f.stages = append(f.stages, s)
	}
	return f, nil
}

func fillError(code handlererrors.ErrorCode, msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$fill (stage)")
}

func fillWrongType(field string, v any, want string) error {
	return fillError(handlererrors.ErrTypeMismatch, "BSON field '$fill."+field+"' is the wrong type '"+
		handlerparams.AliasFromType(v)+"', expected type '"+want+"'")
}

func (f *fill) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	var err error
	for _, s := range f.stages {
		if iter, err = s.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
	}
	return iter, nil
}

var _ aggregations.Stage = (*fill)(nil)
