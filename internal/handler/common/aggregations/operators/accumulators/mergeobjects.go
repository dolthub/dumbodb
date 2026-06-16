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
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// mergeObjectsAccumulator implements the $mergeObjects accumulator for $group.
//
// $mergeObjects in $group combines all documents from the group into a single
// merged document. Later documents in the group override earlier ones for
// duplicate keys. Non-document values are ignored.
type mergeObjectsAccumulator struct {
	expression *aggregations.Expression
	operator   operators.Operator
	isRoot     bool // true when the expression is "$$ROOT"
}

func newMergeObjectsAccumulator(args ...any) (Accumulator, error) {
	if len(args) != 1 {
		return nil, nil
	}

	accumulator := new(mergeObjectsAccumulator)

	switch arg := args[0].(type) {
	case string:
		if arg == "$$ROOT" {
			accumulator.isRoot = true
			break
		}

		if strings.HasPrefix(arg, "$") {
			var err error
			if accumulator.expression, err = aggregations.NewExpression(arg, nil); err != nil {
				// treat as literal (non-expression string)
				accumulator.expression = nil
			}
		}
	case *types.Document:
		if operators.IsOperator(arg) {
			op, err := operators.NewOperator(arg)
			if err != nil {
				return nil, err
			}

			accumulator.operator = op
		}
		// Non-operator documents (object expressions like {k: "$field"}) are not
		// yet fully supported in the accumulator context  -- they would require
		// recursive expression evaluation of each field value.
	}

	return accumulator, nil
}

func (m *mergeObjectsAccumulator) New() Accumulation {
	return &mergeObjectsState{spec: m, result: must.NotFail(types.NewDocument())}
}

type mergeObjectsState struct {
	spec   *mergeObjectsAccumulator
	result *types.Document
}

func (s *mergeObjectsState) Accumulate(doc *types.Document) error {
	m := s.spec

	var val any

	switch {
	case m.isRoot:
		val = doc
	case m.operator != nil:
		v, opErr := m.operator.Process(doc)
		if opErr != nil {
			return nil
		}

		val = v
	case m.expression != nil:
		v, exprErr := m.expression.Evaluate(doc)
		if exprErr != nil {
			return nil
		}

		val = v
	default:
		return nil
	}

	src, ok := val.(*types.Document)
	if !ok {
		return nil
	}

	for _, k := range src.Keys() {
		s.result.Set(k, must.NotFail(src.Get(k)))
	}

	return nil
}

func (s *mergeObjectsState) Result() (any, error) {
	return s.result, nil
}

var (
	_ Accumulator  = (*mergeObjectsAccumulator)(nil)
	_ Accumulation = (*mergeObjectsState)(nil)
)
