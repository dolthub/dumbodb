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

type minAccumulator struct {
	arg operand
}

func newMin(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $min accumulator is a unary operator",
			"$min (accumulator)",
		)
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &minAccumulator{arg: arg}, nil
}

func (m *minAccumulator) New() Accumulation {
	return &extremeState{arg: m.arg, want: types.Less}
}

func (m *maxAccumulator) New() Accumulation {
	return &extremeState{arg: m.arg, want: types.Greater}
}

// extremeState folds the running $min or $max. want is the comparison result
// (types.Less for $min, types.Greater for $max) that replaces the running value.
type extremeState struct {
	arg    operand
	want   types.CompareResult
	result any
}

func (s *extremeState) Accumulate(doc *types.Document) error {
	val, missing, err := s.arg.evaluate(doc)
	if err != nil {
		return err
	}

	// $min and $max ignore missing and null values.
	if _, isNull := val.(types.NullType); missing || isNull {
		return nil
	}

	if s.result == nil {
		s.result = val
		return nil
	}

	if types.CompareOrder(val, s.result, types.Ascending) == s.want {
		s.result = val
	}

	return nil
}

func (s *extremeState) Result() (any, error) {
	if s.result == nil {
		return types.Null, nil
	}

	return s.result, nil
}

type maxAccumulator struct {
	arg operand
}

func newMax(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $max accumulator is a unary operator",
			"$max (accumulator)",
		)
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &maxAccumulator{arg: arg}, nil
}

var (
	_ Accumulator  = (*minAccumulator)(nil)
	_ Accumulator  = (*maxAccumulator)(nil)
	_ Accumulation = (*extremeState)(nil)
)
