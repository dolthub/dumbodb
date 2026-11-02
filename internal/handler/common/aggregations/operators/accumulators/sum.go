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

type sum struct {
	arg operand
}

func newSum(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $sum accumulator is a unary operator",
			"$sum (accumulator)",
		)
	}

	arg, err := newOperand(args[0])
	if err != nil {
		return nil, err
	}

	return &sum{arg: arg}, nil
}

func (s *sum) New() Accumulation {
	return &sumState{spec: s, acc: aggregations.NewNumberSum()}
}

type sumState struct {
	spec *sum
	acc  *aggregations.NumberSum
}

// Accumulate adds the operand's value; non-numeric and missing values
// contribute nothing, so { $sum: 1 } counts documents.
func (st *sumState) Accumulate(doc *types.Document) error {
	value, missing, err := st.spec.arg.evaluate(doc)
	if err != nil || missing {
		return err
	}

	st.acc.Add(value)

	return nil
}

func (st *sumState) Result() (any, error) {
	return st.acc.Result(), nil
}

var (
	_ Accumulator  = (*sum)(nil)
	_ Accumulation = (*sumState)(nil)
)
