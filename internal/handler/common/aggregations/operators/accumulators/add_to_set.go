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
	"errors"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

type addToSetAccumulator struct {
	arg operand
}

func newAddToSet(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $addToSet accumulator is a unary operator",
			"$addToSet (accumulator)",
		)
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &addToSetAccumulator{arg: arg}, nil
}

func (a *addToSetAccumulator) New() Accumulation {
	// $addToSet retains the distinct set of projected values, so its result
	// grows with the number of distinct values. It retains values, never documents.
	return &addToSetState{spec: a, result: types.MakeArray(0)}
}

type addToSetState struct {
	spec   *addToSetAccumulator
	result *types.Array
}

func (s *addToSetState) Accumulate(doc *types.Document) error {
	val, missing, err := s.spec.arg.evaluate(doc)
	if err != nil || missing {
		return err
	}

	if !containsValue(s.result, val) {
		s.result.Append(val)
	}

	return nil
}

func (s *addToSetState) Result() (any, error) {
	return s.result, nil
}

// containsValue returns true if the array already contains a value equal to val
// using BSON comparison semantics.
func containsValue(arr *types.Array, val any) bool {
	iter := arr.Iterator()
	defer iter.Close()

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return false
		}

		if types.CompareForAggregation(v, val) == types.Equal {
			return true
		}
	}

	return false
}

var (
	_ Accumulator  = (*addToSetAccumulator)(nil)
	_ Accumulation = (*addToSetState)(nil)
)
