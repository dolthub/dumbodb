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
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type minAccumulator struct {
	arg operand
	sbe bool
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

// UseSBE implements SBEAccumulator: SBE keeps the last of tied values.
func (m *minAccumulator) UseSBE() error {
	m.sbe = true
	return nil
}

// UseSBE implements SBEAccumulator: SBE keeps the last of tied values.
func (m *maxAccumulator) UseSBE() error {
	m.sbe = true
	return nil
}

func (m *minAccumulator) New() Accumulation {
	return &extremeState{arg: m.arg, want: types.Less, lastTie: m.sbe}
}

func (m *maxAccumulator) New() Accumulation {
	return &extremeState{arg: m.arg, want: types.Greater, lastTie: m.sbe}
}

// NewCollated returns an accumulation that orders strings under cmp.
func (m *minAccumulator) NewCollated(cmp *collation.Comparator) Accumulation {
	return &extremeState{arg: m.arg, want: types.Less, lastTie: m.sbe, cmp: cmp}
}

// NewCollated returns an accumulation that orders strings under cmp.
func (m *maxAccumulator) NewCollated(cmp *collation.Comparator) Accumulation {
	return &extremeState{arg: m.arg, want: types.Greater, lastTie: m.sbe, cmp: cmp}
}

// extremeState folds the running $min or $max. want is the comparison result
// (types.Less for $min, types.Greater for $max) that replaces the running value.
type extremeState struct {
	arg    operand
	want   types.CompareResult
	result any
	cmp    *collation.Comparator
	// lastTie replaces the running value with an equal one, as SBE does.
	lastTie bool
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

	if c := types.CompareOrder(s.cmp.KeyValue(val), s.cmp.KeyValue(s.result), types.Ascending); c == s.want || (s.lastTie && c == types.Equal) {
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
	sbe bool
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
	_ Accumulator = (*minAccumulator)(nil)
	_ Accumulator = (*maxAccumulator)(nil)

	_ CollatedAccumulator = (*minAccumulator)(nil)
	_ CollatedAccumulator = (*maxAccumulator)(nil)
	_ SBEAccumulator      = (*minAccumulator)(nil)
	_ SBEAccumulator      = (*maxAccumulator)(nil)
	_ Accumulation        = (*extremeState)(nil)
)
