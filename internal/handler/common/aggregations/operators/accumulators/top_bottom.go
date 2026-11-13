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

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// topBottomAccumulator implements $top, $bottom, $topN and $bottomN: the
// output of the first or last documents in sortBy order, chosen as rankedSet
// does.
type topBottomAccumulator struct {
	name      string
	output    operand
	paths     []types.Path
	sortTypes []types.SortType
	n         nArg
	single    bool
	bottom    bool
	textMeta  bool
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
		sortDoc, textMeta, err := parseSortBy(sortBy)
		if err != nil {
			return nil, err
		}
		acc.textMeta = textMeta
		for _, key := range sortDoc.Keys() {
			sortType, err := common.GetSortType(key, must.NotFail(sortDoc.Get(key)))
			if err != nil {
				return nil, err
			}
			path, err := types.NewPathFromString(key)
			if err != nil {
				return nil, err
			}
			acc.paths = append(acc.paths, path)
			acc.sortTypes = append(acc.sortTypes, sortType)
		}
		if acc.output, err = newOperand(output); err != nil {
			return nil, err
		}
		if single {
			n = int32(1)
		}
		acc.n = newNArg(n, name)
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

// UseSBE implements SBEAccumulator.
func (a *topBottomAccumulator) UseSBE() error {
	return a.n.useSBE()
}

func (a *topBottomAccumulator) New() Accumulation {
	return &topBottomState{spec: a, mem: keptMemory{sbe: a.n.sbe}}
}

// NewCollated returns an accumulation that orders strings under cmp.
func (a *topBottomAccumulator) NewCollated(cmp *collation.Comparator) Accumulation {
	return &topBottomState{spec: a, mem: keptMemory{sbe: a.n.sbe}, cmp: cmp}
}

// compareKeys returns a comparison of two documents' sort keys in sortBy
// order, with strings compared under cmp when it is set.
func (a *topBottomAccumulator) compareKeys(cmp *collation.Comparator) func(x, y any) int {
	return func(x, y any) int {
		xs, ys := x.([]any), y.([]any)
		for i, order := range a.sortTypes {
			if c := common.CompareSortKeys(cmp.KeyValue(xs[i]), cmp.KeyValue(ys[i]), order); c != types.Equal {
				return int(c)
			}
		}
		return 0
	}
}

type topBottomState struct {
	spec   *topBottomAccumulator
	ranked *rankedSet
	mem    keptMemory
	all    bool
	cmp    *collation.Comparator
}

func (s *topBottomState) keepAll() { s.all = true }

func (s *topBottomState) Accumulate(doc *types.Document) error {
	if s.ranked == nil {
		if s.spec.textMeta {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrMetadataUnavailable, "query requires text score metadata, but it is not available", s.spec.name,
			)
		}
		n, err := s.spec.n.resolve()
		if err != nil {
			return err
		}
		s.ranked = newRankedSet(n, s.spec.compareKeys(s.cmp), s.spec.bottom, s.spec.n.sbe)
		s.ranked.all = s.all
	}
	out, missing, err := s.spec.output.evaluate(doc)
	if err != nil {
		return err
	}
	if missing {
		out = types.Null
	}
	keys := make([]any, len(s.spec.paths))
	for i, p := range s.spec.paths {
		keys[i] = commonpath.SortKeyValue(doc, p)
	}
	return s.ranked.add(keys, out, &s.mem, s.spec.name)
}

func (s *topBottomState) Result() (any, error) {
	var outputs []any
	if s.ranked != nil {
		outputs = s.ranked.sorted()
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
