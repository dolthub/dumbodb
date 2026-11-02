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

package operators

import (
	"errors"
	"fmt"
	"math"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// multiply represents { $multiply: [ <expression>, ... ] }.
type multiply struct {
	args []any
}

func newMultiply(args ...any) (Operator, error) {
	return &multiply{args: args}, nil
}

// Process follows ExpressionMultiply::evaluate: arguments are checked left to
// right, and the first nullish one makes the product null and the first
// non-number is an error.
func (m *multiply) Process(doc *types.Document) (any, error) {
	var product multiplyState
	for _, arg := range m.args {
		v, err := evalArgValue(arg, doc)
		if errors.Is(err, aggregations.ErrMissingValue) {
			return types.Null, nil
		}
		if err != nil {
			return nil, err
		}
		if isNullish(v) {
			return types.Null, nil
		}
		if !isNumber(v) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				fmt.Sprintf("$multiply only supports numeric types, not %s", handlerparams.AliasFromType(v)), "$multiply")
		}
		product.multiply(v)
	}
	return product.value(), nil
}

// Numeric widths in MongoDB's widening order.
const (
	widthInt = iota
	widthLong
	widthDouble
	widthDecimal
)

func numericWidth(v any) int {
	switch v.(type) {
	case int64:
		return widthLong
	case float64:
		return widthDouble
	case types.Decimal128:
		return widthDecimal
	default:
		return widthInt
	}
}

// multiplyState is MongoDB's MultiplyState: double and integral products are
// kept in parallel so the result has the narrowest type, and the running
// product turns into Decimal128 at the first decimal factor.
type multiplyState struct {
	doubleProduct  float64
	longProduct    int64
	decimalProduct *apd.Decimal
	width          int
	started        bool
}

func (s *multiplyState) multiply(v any) {
	if !s.started {
		s.doubleProduct, s.longProduct, s.started = 1, 1, true
	}

	old := s.width
	s.width = max(s.width, numericWidth(v))
	if s.width == widthDecimal {
		if old != widthDecimal {
			if old == widthDouble {
				s.decimalProduct = aggregations.DoubleToAPD(s.doubleProduct)
			} else {
				s.decimalProduct = apd.New(s.longProduct, 0)
			}
		}
		factor, _ := aggregations.NumberToAPD(v)
		s.decimalProduct = aggregations.DecimalMultiply(s.decimalProduct, factor)
		return
	}

	f := toFloat64(v)
	s.doubleProduct *= f
	if s.width != widthDouble {
		n := toInt64(v)
		if math.IsInf(f, 0) || math.IsNaN(f) || mulOverflows(s.longProduct, n) {
			s.width = widthDouble
		} else {
			s.longProduct *= n
		}
	}
}

func (s *multiplyState) value() any {
	if !s.started {
		return int32(1)
	}
	switch s.width {
	case widthDouble:
		return s.doubleProduct
	case widthLong:
		return s.longProduct
	case widthDecimal:
		return aggregations.APDToDecimal128(s.decimalProduct)
	default:
		if s.longProduct >= math.MinInt32 && s.longProduct <= math.MaxInt32 {
			return int32(s.longProduct)
		}
		return s.longProduct
	}
}

func mulOverflows(a, b int64) bool {
	if a == 0 || b == 0 {
		return false
	}
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return true
	}
	return a*b/b != a
}

var _ Operator = (*multiply)(nil)
