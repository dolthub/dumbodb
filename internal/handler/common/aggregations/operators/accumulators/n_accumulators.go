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
	"math"
	"slices"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// nArg is the 'n' of an N accumulator. mongod validates a constant n while
// parsing and evaluates any other once per group, where no document field is
// available.
type nArg struct {
	expr  any
	value int64
}

func newNArg(expr any) (nArg, error) {
	if !operators.IsConstantExpression(expr) {
		return nArg{expr: expr}, nil
	}
	n, err := evalN(expr, handlererrors.ErrNAccumulatorNInvalid)
	if err != nil {
		return nArg{}, err
	}
	return nArg{value: n}, nil
}

func (a nArg) resolve() (int64, error) {
	if a.expr == nil {
		return a.value, nil
	}
	return evalN(a.expr, handlererrors.ErrNAccumulatorNInvalidAtRuntime)
}

func evalN(expr any, code handlererrors.ErrorCode) (int64, error) {
	v, missing, err := operators.EvalOperand(expr, types.MakeDocument(0))
	if err != nil {
		return 0, err
	}
	if n, ok := positiveInt64(v); ok && !missing {
		return n, nil
	}
	return 0, handlererrors.NewCommandErrorMsgWithArgument(
		code, "parameter 'n' must be coercible to a positive 64-bit integer", "n",
	)
}

func positiveInt64(v any) (int64, bool) {
	var n int64
	switch x := v.(type) {
	case int32:
		n = int64(x)
	case int64:
		n = x
	case float64:
		if x != math.Trunc(x) || x >= math.MaxInt64 || x < math.MinInt64 {
			return 0, false
		}
		n = int64(x)
	case types.Decimal128:
		i, ok := aggregations.Decimal128ToInt64(x)
		if !ok {
			return 0, false
		}
		n = i
	default:
		return 0, false
	}
	return n, n > 0
}

func specNotObjectError(code handlererrors.ErrorCode, op string, v any) error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		code, fmt.Sprintf("specification must be an object; found %s: %s", op, types.FormatAnyValue(v)), op,
	)
}

// nAccumulator implements $firstN, $lastN, $minN and $maxN.
type nAccumulator struct {
	name  string
	input operand
	n     nArg
}

func newNAccumulatorFunc(name string) newAccumulatorFunc {
	return func(args ...any) (Accumulator, error) {
		spec, ok := args[0].(*types.Document)
		if !ok {
			return nil, specNotObjectError(handlererrors.ErrNAccumulatorSpecNotObject, name, args[0])
		}
		var input, n any
		for _, key := range spec.Keys() {
			switch key {
			case "input":
				input = must.NotFail(spec.Get(key))
			case "n":
				n = must.NotFail(spec.Get(key))
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrNAccumulatorUnknownArgument, "Unknown argument for 'n' operator: "+key, name,
				)
			}
		}
		if n == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNAccumulatorMissingN, "Missing value for 'n'", name,
			)
		}
		if input == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNAccumulatorMissingInput, "Missing value for 'input'", name,
			)
		}
		inputOperand, err := newOperand(input)
		if err != nil {
			return nil, err
		}
		nValue, err := newNArg(n)
		if err != nil {
			return nil, err
		}
		return &nAccumulator{name: name, input: inputOperand, n: nValue}, nil
	}
}

func (a *nAccumulator) New() Accumulation {
	return &nState{spec: a}
}

type nState struct {
	spec   *nAccumulator
	values []any
	n      int64
	ready  bool
}

func (s *nState) Accumulate(doc *types.Document) error {
	if !s.ready {
		n, err := s.spec.n.resolve()
		if err != nil {
			return err
		}
		s.n, s.ready = n, true
	}
	v, missing, err := s.spec.input.evaluate(doc)
	if err != nil {
		return err
	}
	switch s.spec.name {
	case "$firstN":
		if int64(len(s.values)) >= s.n {
			return nil
		}
	case "$minN", "$maxN":
		if missing || isNull(v) {
			return nil
		}
	}
	if missing {
		v = types.Null
	}
	s.values = append(s.values, v)
	if s.spec.name == "$lastN" && int64(len(s.values)) > s.n {
		s.values = s.values[1:]
	}
	return nil
}

func (s *nState) Result() (any, error) {
	values := s.values
	if s.spec.name == "$minN" || s.spec.name == "$maxN" {
		slices.SortStableFunc(values, func(a, b any) int {
			c := int(types.CompareOrder(a, b, types.Ascending))
			if s.spec.name == "$maxN" {
				c = -c
			}
			return c
		})
		values = values[:min(int64(len(values)), s.n)]
	}
	out := types.MakeArray(len(values))
	for _, v := range values {
		out.Append(v)
	}
	return out, nil
}

func isNull(v any) bool {
	_, ok := v.(types.NullType)
	return ok
}

var (
	_ Accumulator  = (*nAccumulator)(nil)
	_ Accumulation = (*nState)(nil)
)
