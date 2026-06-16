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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

type sum struct {
	expression *aggregations.Expression
	operator   operators.Operator
	number     any
}

func newSum(args ...any) (Accumulator, error) {
	accumulator := new(sum)

	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $sum accumulator is a unary operator",
			"$sum (accumulator)",
		)
	}

	for _, arg := range args {
		switch arg := arg.(type) {
		case *types.Document:
			if !operators.IsOperator(arg) {
				accumulator.number = int32(0)
				break
			}

			op, err := operators.NewOperator(arg)
			if err != nil {
				var opErr operators.OperatorError
				if !errors.As(err, &opErr) {
					return nil, lazyerrors.Error(err)
				}

				return nil, opErr
			}

			accumulator.operator = op
		case float64:
			accumulator.number = arg
		case string:
			var err error
			if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
				// $sum returns 0 on non-existent field.
				accumulator.number = int32(0)
			}
		case int32, int64:
			accumulator.number = arg
		default:
			accumulator.number = int32(0)
			// $sum returns 0 on non-numeric field
		}
	}

	return accumulator, nil
}

func (s *sum) New() Accumulation {
	return &sumState{spec: s, acc: aggregations.NewNumberSum()}
}

type sumState struct {
	spec *sum
	acc  *aggregations.NumberSum
}

func (st *sumState) Accumulate(doc *types.Document) error {
	s := st.spec

	switch {
	case s.operator != nil:
		v, err := s.operator.Process(doc)
		if err != nil {
			return err
		}

		st.acc.Add(v)

		return nil

	case s.expression != nil:
		// sum fields that exist
		if value, err := s.expression.Evaluate(doc); err == nil {
			st.acc.Add(value)
		}

		return nil
	}

	// Constant: { $sum: 1 } folds the same value per document (equivalent to
	// $count). Non-numeric constants contribute nothing, so Result stays int32(0).
	switch s.number.(type) {
	case float64, int32, int64:
		st.acc.Add(s.number)
	}

	return nil
}

func (st *sumState) Result() (any, error) {
	return st.acc.Result(), nil
}

var (
	_ Accumulator  = (*sum)(nil)
	_ Accumulation = (*sumState)(nil)
)
