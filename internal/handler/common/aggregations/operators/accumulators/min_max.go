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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// minAccumulator represents $min aggregation accumulator.
type minAccumulator struct {
	expression *aggregations.Expression
	number     any
}

// newMin creates a new $min accumulator.
func newMin(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $min accumulator is a unary operator",
			"$min (accumulator)",
		)
	}

	accumulator := new(minAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			// non-expression string: treat as constant
			accumulator.number = arg
		}
	default:
		accumulator.number = arg
	}

	return accumulator, nil
}

// Accumulate implements Accumulator interface.
func (m *minAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	var result any

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var val any

		if m.expression != nil {
			v, evalErr := m.expression.Evaluate(doc)
			if evalErr != nil {
				// missing field: skip (treat as absent)
				continue
			}

			val = v
		} else {
			val = m.number
		}

		// Skip null values ($min ignores nulls)
		if _, isNull := val.(types.NullType); isNull {
			continue
		}

		if result == nil {
			result = val
			continue
		}

		if types.CompareOrder(val, result, types.Ascending) == types.Less {
			result = val
		}
	}

	if result == nil {
		return types.Null, nil
	}

	return result, nil
}

// maxAccumulator represents $max aggregation accumulator.
type maxAccumulator struct {
	expression *aggregations.Expression
	number     any
}

// newMax creates a new $max accumulator.
func newMax(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $max accumulator is a unary operator",
			"$max (accumulator)",
		)
	}

	accumulator := new(maxAccumulator)

	switch arg := args[0].(type) {
	case string:
		var err error
		if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
			accumulator.number = arg
		}
	default:
		accumulator.number = arg
	}

	return accumulator, nil
}

// Accumulate implements Accumulator interface.
func (m *maxAccumulator) Accumulate(iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	var result any

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var val any

		if m.expression != nil {
			v, evalErr := m.expression.Evaluate(doc)
			if evalErr != nil {
				// missing field: skip (treat as absent)
				continue
			}

			val = v
		} else {
			val = m.number
		}

		// Skip null values ($max ignores nulls)
		if _, isNull := val.(types.NullType); isNull {
			continue
		}

		if result == nil {
			result = val
			continue
		}

		if types.CompareOrder(val, result, types.Ascending) == types.Greater {
			result = val
		}
	}

	if result == nil {
		return types.Null, nil
	}

	return result, nil
}

// check interfaces
var (
	_ Accumulator = (*minAccumulator)(nil)
	_ Accumulator = (*maxAccumulator)(nil)
)
