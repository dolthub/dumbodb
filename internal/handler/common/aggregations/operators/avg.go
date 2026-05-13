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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// avgOp represents the $avg expression operator.
//
// When used outside of $group (e.g., in $addFields or $project), $avg computes
// the average of numeric values. It accepts:
//   - a single field path: averages elements of an array field, or returns the value if numeric
//   - an array literal: averages all elements
//   - multiple arguments: averages all values
type avgOp struct {
	args []any
}

func newAvgOp(args ...any) (Operator, error) {
	return &avgOp{args: args}, nil
}

// Process implements Operator interface.
// It evaluates all arguments, collects numeric values, and returns their average.
// Non-numeric values are ignored. Returns null when no numeric values are found.
func (a *avgOp) Process(doc *types.Document) (any, error) {
	var numbers []any

	for _, arg := range a.args {
		val, err := evalArgValue(arg, doc)
		if err != nil {
			// skip args that fail to evaluate
			continue
		}

		switch v := val.(type) {
		case *types.Array:
			// When the argument evaluates to an array, average the array elements.
			iter := v.Iterator()
			defer iter.Close()

			for {
				_, elem, iterErr := iter.Next()
				if errors.Is(iterErr, iterator.ErrIteratorDone) {
					break
				}

				if iterErr != nil {
					return nil, lazyerrors.Error(iterErr)
				}

				numbers = append(numbers, elem)
			}
		default:
			numbers = append(numbers, val)
		}
	}

	return aggregations.AvgNumbers(numbers...), nil
}

var (
	_ Operator = (*avgOp)(nil)
)
