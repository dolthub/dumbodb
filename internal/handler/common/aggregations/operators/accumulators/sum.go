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
	"math"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type sum struct {
	arg operand

	// constant is a literal int, long or double argument, which the classic
	// engine sums with plain typed arithmetic.
	constant any
	sbe      bool
}

// UseSBE implements SBEAccumulator.
func (s *sum) UseSBE() error {
	s.sbe = true
	return nil
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

	return &sum{arg: arg, constant: constantAddend(args[0])}, nil
}

// constantAddend returns a literal int32, int64 or float64 argument, or nil.
func constantAddend(arg any) any {
	if doc, ok := arg.(*types.Document); ok && doc.Len() == 1 && doc.Keys()[0] == "$literal" {
		arg = doc.Values()[0]
	}
	switch arg.(type) {
	case int32, int64, float64:
		return arg
	}
	return nil
}

func (s *sum) New() Accumulation {
	if s.constant != nil && !s.sbe {
		return &constantSumState{addend: s.constant, total: zeroOfType(s.constant)}
	}
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

// constantSumState is the classic engine's sum of a constant: int overflows
// to long, long to double, and doubles add without compensation.
type constantSumState struct {
	addend any
	total  any
}

func (st *constantSumState) Accumulate(*types.Document) error {
	switch total := st.total.(type) {
	case int32:
		n := int64(total) + int64(st.addend.(int32))
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			st.total = int32(n)
			return nil
		}
		st.total = int64(total)
		return st.Accumulate(nil)
	case int64:
		addend := toInt64(st.addend)
		if n, ok := addInt64(total, addend); ok {
			st.total = n
			return nil
		}
		st.total = float64(total)
		return st.Accumulate(nil)
	case float64:
		st.total = total + toFloat64(st.addend)
	}
	return nil
}

func (st *constantSumState) Result() (any, error) {
	return st.total, nil
}

func zeroOfType(v any) any {
	switch v.(type) {
	case int32:
		return int32(0)
	case int64:
		return int64(0)
	default:
		return float64(0)
	}
}

func toInt64(v any) int64 {
	switch v := v.(type) {
	case int32:
		return int64(v)
	case int64:
		return v
	default:
		return int64(v.(float64))
	}
}

func toFloat64(v any) float64 {
	switch v := v.(type) {
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return v.(float64)
	}
}

func addInt64(a, b int64) (int64, bool) {
	n := a + b
	return n, (n > a) == (b > 0)
}

var (
	_ Accumulator    = (*sum)(nil)
	_ SBEAccumulator = (*sum)(nil)
	_ Accumulation   = (*sumState)(nil)
	_ Accumulation   = (*constantSumState)(nil)
)
