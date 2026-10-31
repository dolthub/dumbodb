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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type pushAccumulator struct {
	arg operand
}

func newPush(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $push accumulator is a unary operator",
			"$push (accumulator)",
		)
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &pushAccumulator{arg: arg}, nil
}

func (p *pushAccumulator) New() Accumulation {
	// $push must retain every projected value, so its result array grows with
	// the group size. It retains values, never documents.
	return &pushState{spec: p, result: types.MakeArray(0)}
}

type pushState struct {
	spec   *pushAccumulator
	result *types.Array
}

// Accumulate appends the operand's value; MongoDB's $push skips a missing one.
func (s *pushState) Accumulate(doc *types.Document) error {
	val, missing, err := s.spec.arg.evaluate(doc)
	if err != nil || missing {
		return err
	}

	s.result.Append(val)

	return nil
}

func (s *pushState) Result() (any, error) {
	return s.result, nil
}

var (
	_ Accumulator  = (*pushAccumulator)(nil)
	_ Accumulation = (*pushState)(nil)
)
