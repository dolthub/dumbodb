// Copyright 2021 FerretDB Inc.
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

// pushAccumulator represents $push aggregation accumulator.
type pushAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
}

// newPush creates a new $push accumulator.
func newPush(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $push accumulator is a unary operator",
			"$push (accumulator)",
		)
	}

	accumulator := new(pushAccumulator)

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

// Accumulate implements Accumulator interface.
func (p *pushAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	result := types.MakeArray(0)

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if p.isConst {
			result.Append(p.constant)
			continue
		}

		val, evalErr := p.expression.Evaluate(doc)
		if evalErr != nil {
			// missing field: push null
			result.Append(types.Null)
			continue
		}

		result.Append(val)
	}

	return result, nil
}

// check interfaces
var (
	_ Accumulator = (*pushAccumulator)(nil)
)
