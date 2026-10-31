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

// Package operators provides aggregation operators.
package operators

import (
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

type typeOp struct {
	param any
}

func newType(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$type",
			fmt.Sprintf("Expression $type takes exactly 1 arguments. %d were passed in.", len(args)),
		)
	}

	return &typeOp{
		param: args[0],
	}, nil
}

// Process returns the type alias of the evaluated argument, "missing" for
// the missing value.
func (t *typeOp) Process(doc *types.Document) (any, error) {
	v, missing, err := EvalOperand(t.param, doc)
	if err != nil {
		var opErr OperatorError
		if errors.As(err, &opErr) && opErr.Code() == ErrInvalidExpression {
			opErr.code = ErrInvalidNestedExpression
			return nil, opErr
		}
		return nil, err
	}
	if missing {
		return "missing", nil
	}
	return handlerparams.AliasFromType(v), nil
}

var (
	_ Operator = (*typeOp)(nil)
)
