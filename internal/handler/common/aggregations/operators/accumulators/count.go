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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

type count struct{}

func newCount(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupUnaryOperator,
			"The $count accumulator is a unary operator",
			"$count (accumulator)",
		)
	}

	doc, ok := args[0].(*types.Document)

	if !ok || doc.Len() > 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$count takes no arguments, i.e. $count:{}",
			"$count (accumulator)",
		)
	}

	return new(count), nil
}

func (c *count) New() Accumulation { return &countState{} }

type countState struct {
	n int32
}

func (s *countState) Accumulate(_ *types.Document) error {
	s.n++
	return nil
}

func (s *countState) Result() (any, error) {
	return s.n, nil
}

var (
	_ Accumulator = (*count)(nil)
	_ Accumulation = (*countState)(nil)
)
