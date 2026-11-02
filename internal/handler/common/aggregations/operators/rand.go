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
	"math/rand/v2"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// randOp represents { $rand: {} }.
type randOp struct{}

// newRand follows ExpressionRandom::parse; its argument is passed whole, and
// an array counts as an object, as in BSON.
func newRand(args ...any) (Operator, error) {
	switch arg := args[0].(type) {
	case *types.Document:
		if arg.Len() == 0 {
			return randOp{}, nil
		}
	case *types.Array:
		if arg.Len() == 0 {
			return randOp{}, nil
		}
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrExpectedObject,
			"invalid parameter: expected an object ($rand)", "$rand")
	}
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRandArguments,
		"$rand does not currently accept arguments", "$rand")
}

// Process returns a double in [0, 1).
func (randOp) Process(*types.Document) (any, error) {
	return rand.Float64(), nil
}

var _ Operator = randOp{}
