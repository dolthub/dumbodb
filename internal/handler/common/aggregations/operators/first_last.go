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
	"fmt"

	"github.com/dolthub/dumbodb/internal/types"
)

// When used outside of $group, $first acts as an array expression operator:
//   - If the evaluated argument is an array, returns its first element (null if empty).
//   - If the evaluated argument is null/missing, returns null.
//   - Otherwise returns the value as-is.
type firstOp struct{ arg any }

func newFirstOp(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$first",
			fmt.Sprintf("Expression $first takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &firstOp{arg: args[0]}, nil
}

func (op *firstOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return v, nil
	}

	if arr.Len() == 0 {
		return types.Null, nil
	}

	first, err := arr.Get(0)
	if err != nil {
		return types.Null, nil
	}

	return first, nil
}

var _ Operator = (*firstOp)(nil)

// When used outside of $group, $last acts as an array expression operator:
//   - If the evaluated argument is an array, returns its last element (null if empty).
//   - If the evaluated argument is null/missing, returns null.
//   - Otherwise returns the value as-is.
type lastOp struct{ arg any }

func newLastOp(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$last",
			fmt.Sprintf("Expression $last takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &lastOp{arg: args[0]}, nil
}

func (op *lastOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return v, nil
	}

	n := arr.Len()
	if n == 0 {
		return types.Null, nil
	}

	last, err := arr.Get(n - 1)
	if err != nil {
		return types.Null, nil
	}

	return last, nil
}

var _ Operator = (*lastOp)(nil)
