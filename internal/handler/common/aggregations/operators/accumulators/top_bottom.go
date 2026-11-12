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

package accumulators

import (
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// topBottomAccumulator implements $top, $bottom, $topN and $bottomN: the
// output of the first or last documents in sortBy order. Documents that tie
// keep their input order, and a later one never displaces an earlier tie.
type topBottomAccumulator struct {
	name     string
	output   operand
	sortBy   *types.Document
	reversed *types.Document
	n        nArg
	single   bool
	bottom   bool
	textMeta bool
}

func newTopBottomFunc(name string) newAccumulatorFunc {
	single := name == "$top" || name == "$bottom"
	return func(args ...any) (Accumulator, error) {
		spec, ok := args[0].(*types.Document)
		if !ok {
			return nil, specNotObjectError(handlererrors.ErrTopSpecNotObject, name, args[0])
		}
		var output, sortBy, n any
		for _, key := range spec.Keys() {
			v := must.NotFail(spec.Get(key))
			switch {
			case key == "output":
				output = v
			case key == "sortBy":
				sortBy = v
			case key == "n" && !single:
				n = v
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTopUnknownArgument, fmt.Sprintf("Unknown argument to %s '%s'", name, key), name,
				)
			}
		}
		if !single && n == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTopMissingN, "Missing value for 'n'", name,
			)
		}
		if output == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTopMissingOutput, "Missing value for 'output'", name,
			)
		}
		if sortBy == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTopMissingSortBy, "Missing value for 'sortBy'", name,
			)
		}

		acc := &topBottomAccumulator{name: name, single: single, bottom: strings.HasPrefix(name, "$bottom")}
		var err error
		if acc.sortBy, acc.textMeta, err = parseSortBy(sortBy); err != nil {
			return nil, err
		}
		acc.reversed = reverseSort(acc.sortBy)
		if acc.output, err = newOperand(output); err != nil {
			return nil, err
		}
		if single {
			acc.n = nArg{value: 1}
		} else if acc.n, err = newNArg(n); err != nil {
			return nil, err
		}
		return acc, nil
	}
}

// parseSortBy validates sortBy as mongod's sort pattern parser does, and
// reports whether it sorts by text score.
func parseSortBy(v any) (*types.Document, bool, error) {
	sortBy, ok := v.(*types.Document)
	if !ok {
		return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrExpectedObject, "invalid parameter: expected an object (sortBy)", "sortBy",
		)
	}
	textMeta := false
	for _, key := range sortBy.Keys() {
		if strings.HasPrefix(key, "$") {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFieldPathInvalidName,
				"Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "+
					"FieldPath field names may not start with '$', given '"+key+"'.",
				"sortBy",
			)
		}
		if d, ok := must.NotFail(sortBy.Get(key)).(*types.Document); ok && d.Has("$meta") {
			textMeta = true
		}
	}
	validated, err := common.ValidateSortDocument(sortBy)
	if err != nil {
		return nil, false, err
	}
	if validated == nil {
		validated = types.MakeDocument(0)
	}
	return validated, textMeta, nil
}

// reverseSort returns sortBy with each direction flipped.
func reverseSort(sortBy *types.Document) *types.Document {
	res := types.MakeDocument(sortBy.Len())
	for _, key := range sortBy.Keys() {
		v := must.NotFail(sortBy.Get(key))
		switch d := v.(type) {
		case int64:
			v = -d
		case int32:
			v = -d
		case float64:
			v = -d
		}
		res.Set(key, v)
	}
	return res
}

func (a *topBottomAccumulator) New() Accumulation {
	return &topBottomState{spec: a}
}

type topBottomState struct {
	spec *topBottomAccumulator
	docs []*types.Document
	n    int64
}

func (s *topBottomState) Accumulate(doc *types.Document) error {
	if s.docs == nil {
		if s.spec.textMeta {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrMetadataUnavailable, "query requires text score metadata, but it is not available", s.spec.name,
			)
		}
		n, err := s.spec.n.resolve()
		if err != nil {
			return err
		}
		s.n = n
	}
	s.docs = append(s.docs, doc)
	return nil
}

func (s *topBottomState) Result() (any, error) {
	order := s.spec.sortBy
	if s.spec.bottom {
		order = s.spec.reversed
	}
	if err := common.SortDocuments(s.docs, order); err != nil {
		return nil, err
	}
	selected := s.docs[:min(int64(len(s.docs)), s.n)]
	if s.spec.bottom {
		if err := common.SortDocuments(selected, s.spec.sortBy); err != nil {
			return nil, err
		}
	}

	outputs := make([]any, 0, len(selected))
	for _, doc := range selected {
		v, missing, err := s.spec.output.evaluate(doc)
		if err != nil {
			return nil, err
		}
		if missing {
			v = types.Null
		}
		outputs = append(outputs, v)
	}

	if s.spec.single {
		if len(outputs) == 0 {
			return types.Null, nil
		}
		return outputs[0], nil
	}
	out := types.MakeArray(len(outputs))
	for _, v := range outputs {
		out.Append(v)
	}
	return out, nil
}

var (
	_ Accumulator  = (*topBottomAccumulator)(nil)
	_ Accumulation = (*topBottomState)(nil)
)
