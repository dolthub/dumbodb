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

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// avg represents $avg aggregation accumulator.
type avg struct {
	expression *aggregations.Expression
	number     any
}

// newAvg creates a new $avg accumulator.
func newAvg(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $avg accumulator is a unary operator",
			"$avg (accumulator)",
		)
	}

	accumulator := new(avg)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.number = int32(0)
		}
	case float64, int32, int64:
		accumulator.number = arg
	default:
		accumulator.number = int32(0)
	}

	return accumulator, nil
}

// Accumulate implements Accumulator interface.
func (a *avg) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	var numbers []any

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if a.expression != nil {
			value, err := a.expression.Evaluate(doc)
			if err == nil {
				numbers = append(numbers, value)
			}

			continue
		}

		switch number := a.number.(type) {
		case float64, int32, int64:
			numbers = append(numbers, number)
		}
	}

	return aggregations.AvgNumbers(numbers...), nil
}


// check interfaces
var (
	_ Accumulator = (*avg)(nil)
)
