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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// operand is an accumulator's argument: any aggregation expression.
type operand struct {
	expr any
}

// newOperand validates an operator at the top of expr up front, as MongoDB
// parses accumulator arguments when it parses the pipeline.
func newOperand(expr any) (operand, error) {
	if doc, ok := expr.(*types.Document); ok && operators.IsOperator(doc) {
		if _, err := operators.NewOperator(doc); err != nil {
			var opErr operators.OperatorError
			if errors.As(err, &opErr) {
				return operand{}, opErr
			}
			return operand{}, lazyerrors.Error(err)
		}
	}
	return operand{expr: expr}, nil
}

// evaluate returns the operand's value for doc; missing reports MongoDB's
// missing value, which each accumulator treats in its own way.
func (o operand) evaluate(doc *types.Document) (value any, missing bool, err error) {
	return operators.EvalOperand(o.expr, doc)
}
