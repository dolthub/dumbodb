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

// firstAccumulator represents $first aggregation accumulator.
type firstAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
}

// newFirst creates a new $first accumulator.
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

// Accumulate implements Accumulator interface.
func (f *firstAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if f.isConst {
			return f.constant, nil
		}

		val, evalErr := f.expression.Evaluate(doc)
		if evalErr != nil {
			return types.Null, nil
		}

		return val, nil
	}

	return types.Null, nil
}

// lastAccumulator represents $last aggregation accumulator.
type lastAccumulator struct {
	expression *aggregations.Expression
	constant   any
	isConst    bool
}

// newLast creates a new $last accumulator.
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

// Accumulate implements Accumulator interface.
func (l *lastAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	var result any = types.Null
	var hasDoc bool

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		hasDoc = true

		if l.isConst {
			result = l.constant
			continue
		}

		val, evalErr := l.expression.Evaluate(doc)
		if evalErr != nil {
			result = types.Null
		} else {
			result = val
		}
	}

	if !hasDoc {
		return types.Null, nil
	}

	return result, nil
}

// check interfaces
var (
	_ Accumulator = (*firstAccumulator)(nil)
	_ Accumulator = (*lastAccumulator)(nil)
)
