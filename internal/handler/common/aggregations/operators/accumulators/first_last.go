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
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type firstAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
}

func newFirst(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $first accumulator is a unary operator",
			"$first (accumulator)",
		)
	}

	accumulator := new(firstAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.constant = arg
			accumulator.isConst = true
		}
	default:
		accumulator.constant = arg
		accumulator.isConst = true
	}

	return accumulator, nil
}

func (f *firstAccumulator) New() Accumulation {
	return &firstState{spec: f}
}

type firstState struct {
	spec   *firstAccumulator
	result any
	seen   bool
}

func (s *firstState) Accumulate(doc *types.Document) error {
	if s.seen {
		return nil
	}

	s.seen = true

	if s.spec.isConst {
		s.result = s.spec.constant
		return nil
	}

	val, evalErr := s.spec.expression.Evaluate(doc)
	if evalErr != nil {
		s.result = types.Null
		return nil
	}

	s.result = val

	return nil
}

func (s *firstState) Result() (any, error) {
	if !s.seen {
		return types.Null, nil
	}

	return s.result, nil
}

type lastAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
}

func newLast(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $last accumulator is a unary operator",
			"$last (accumulator)",
		)
	}

	accumulator := new(lastAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.constant = arg
			accumulator.isConst = true
		}
	default:
		accumulator.constant = arg
		accumulator.isConst = true
	}

	return accumulator, nil
}

func (l *lastAccumulator) New() Accumulation {
	return &lastState{spec: l, result: types.Null}
}

type lastState struct {
	spec   *lastAccumulator
	result any
	seen   bool
}

func (s *lastState) Accumulate(doc *types.Document) error {
	s.seen = true

	if s.spec.isConst {
		s.result = s.spec.constant
		return nil
	}

	val, evalErr := s.spec.expression.Evaluate(doc)
	if evalErr != nil {
		s.result = types.Null
	} else {
		s.result = val
	}

	return nil
}

func (s *lastState) Result() (any, error) {
	if !s.seen {
		return types.Null, nil
	}

	return s.result, nil
}

var (
	_ Accumulator  = (*firstAccumulator)(nil)
	_ Accumulator  = (*lastAccumulator)(nil)
	_ Accumulation = (*firstState)(nil)
	_ Accumulation = (*lastState)(nil)
)
