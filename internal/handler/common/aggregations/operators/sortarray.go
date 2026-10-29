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

package operators

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/stdsort"
)

// sortArrayOp represents { $sortArray: { input: <expr>, sortBy: <spec> } }.
type sortArrayOp struct {
	input any

	// fields is nil to sort whole values in order (1 or -1).
	fields []sortArrayField
	order  int
}

type sortArrayField struct {
	path  []string
	order int
}

// newSortArray follows ExpressionSortArray::parse.
func newSortArray(args ...any) (Operator, error) {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayNotObject,
			"$sortArray requires an object as an argument, found: "+handlerparams.AliasFromType(args[0]), "$sortArray")
	}

	op := new(sortArrayOp)
	var hasInput, hasSortBy bool
	iter := spec.Iterator()
	defer iter.Close()
	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch key {
		case "input":
			op.input, hasInput = value, true
		case "sortBy":
			if err = op.parseSortBy(value); err != nil {
				return nil, err
			}
			hasSortBy = true
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayUnknownField,
				"$sortArray found an unknown argument: "+key, "$sortArray")
		}
	}
	if !hasInput {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayNoInput,
			"$sortArray requires 'input' to be specified", "$sortArray")
	}
	if !hasSortBy {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayNoSortBy,
			"$sortArray requires 'sortBy' to be specified", "$sortArray")
	}
	return op, nil
}

// parseSortBy is createSortSpecObject with pattern_cmp::checkSortClause.
func (op *sortArrayOp) parseSortBy(sortBy any) error {
	badPattern := func(msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayBadPattern, "BadValue: "+msg, "$sortArray")
	}

	switch sortBy := sortBy.(type) {
	case *types.Document:
		if sortBy.Len() == 0 {
			return badPattern("The sort pattern is empty when it should be a set of fields.")
		}
		op.fields = nil
		for _, key := range sortBy.Keys() {
			order, ok := sortOrder(must.NotFail(sortBy.Get(key)))
			if !ok {
				return badPattern("The sort element value must be either 1 or -1")
			}
			path := strings.Split(key, ".")
			for _, part := range path {
				if part == "" {
					if key == "" {
						return badPattern("The sort field cannot be empty")
					}
					return badPattern("The sort field is a dotted field but has an empty part: " + key)
				}
			}
			op.fields = append(op.fields, sortArrayField{path: path, order: order})
		}
		return nil

	default:
		if !isNumber(sortBy) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayBadSortBy,
				"The $sort is invalid: use 1/-1 to sort the whole element, or {field:1/-1} to sort embedded fields", "$sortArray")
		}
		order, ok := sortOrder(sortBy)
		if !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayBadOrder,
				"The $sort element value must be either 1 or -1", "$sortArray")
		}
		op.fields, op.order = nil, order
		return nil
	}
}

// sortOrder returns 1 or -1 for a number equal to it.
func sortOrder(v any) (int, bool) {
	if !isNumber(v) {
		return 0, false
	}
	f := toFloat64(v)
	if d, ok := v.(types.Decimal128); ok {
		f, _ = aggregations.DecimalToAPD(d).Float64()
	}
	switch f {
	case 1, -1:
		return int(f), true
	default:
		return 0, false
	}
}

// Process follows ExpressionSortArray::evaluate.
func (op *sortArrayOp) Process(doc *types.Document) (any, error) {
	input, err := evalArgValue(op.input, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(input) {
		return types.Null, nil
	}
	arr, ok := input.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSortArrayInputNotArray,
			fmt.Sprintf("The input argument to $sortArray must be an array, but was of type: %s", handlerparams.AliasFromType(input)), "$sortArray")
	}
	if arr.Len() < 2 {
		return arr, nil
	}

	values := make([]any, arr.Len())
	for i := range values {
		values[i] = must.NotFail(arr.Get(i))
	}
	stdsort.Sort(values, op.less)
	return must.NotFail(types.NewArray(values...)), nil
}

// less is PatternValueCmp: whole values compare in BSON order; otherwise
// each element is keyed by the pattern's fields, missing ones and every
// field of a non-document being null.
func (op *sortArrayOp) less(a, b any) bool {
	if op.fields == nil {
		if op.order < 0 {
			a, b = b, a
		}
		return types.CompareOrder(a, b, types.Ascending) == types.Less
	}

	for _, f := range op.fields {
		c := types.CompareOrder(sortArrayKey(a, f.path), sortArrayKey(b, f.path), types.Ascending)
		if c == types.Equal {
			continue
		}
		return (c == types.Less) == (f.order > 0)
	}
	return false
}

// sortArrayKey is BSONObj::getFieldDotted: arrays are only entered by
// index, and anything missing is null.
func sortArrayKey(v any, path []string) any {
	for _, part := range path {
		switch container := v.(type) {
		case *types.Document:
			next, err := container.Get(part)
			if err != nil {
				return types.Null
			}
			v = next
		case *types.Array:
			i, err := strconv.Atoi(part)
			if err != nil || strconv.Itoa(i) != part {
				return types.Null
			}
			next, err := container.Get(i)
			if err != nil {
				return types.Null
			}
			v = next
		default:
			return types.Null
		}
	}
	return v
}

var _ Operator = (*sortArrayOp)(nil)
