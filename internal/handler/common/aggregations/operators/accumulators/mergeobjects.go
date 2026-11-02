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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type mergeObjectsAccumulator struct {
	arg operand
}

func newMergeObjectsAccumulator(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, unaryOperatorError("$mergeObjects")
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &mergeObjectsAccumulator{arg: arg}, nil
}

func (m *mergeObjectsAccumulator) New() Accumulation {
	return &mergeObjectsState{spec: m, result: must.NotFail(types.NewDocument())}
}

type mergeObjectsState struct {
	spec   *mergeObjectsAccumulator
	result *types.Document
}

// Accumulate merges the operand's fields into the result. Missing and null
// operands are skipped; anything else that is not an object is an error, with
// the code MongoDB's $group (evaluated in SBE) reports.
func (s *mergeObjectsState) Accumulate(doc *types.Document) error {
	val, missing, err := s.spec.arg.evaluate(doc)
	if err != nil || missing {
		return err
	}

	switch src := val.(type) {
	case types.NullType, types.UndefinedType:
		return nil
	case *types.Document:
		for _, k := range src.Keys() {
			s.result.Set(k, must.NotFail(src.Get(k)))
		}
		return nil
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMergeObjectsNotObject,
			fmt.Sprintf("$mergeObjects only supports objects, found %s", handlerparams.AliasFromType(val)), "$mergeObjects")
	}
}

func (s *mergeObjectsState) Result() (any, error) {
	return s.result, nil
}

var (
	_ Accumulator  = (*mergeObjectsAccumulator)(nil)
	_ Accumulation = (*mergeObjectsState)(nil)
)
