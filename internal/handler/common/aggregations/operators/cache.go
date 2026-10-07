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
	"sync"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/identitycache"
)

// builtOperators reuses operators across evaluations: Process keeps no state.
var builtOperators identitycache.Cache[types.Document, Operator]

func cachedOperator(doc *types.Document) (Operator, error) {
	if op, ok := builtOperators.Get(doc); ok {
		return op, nil
	}
	op, err := NewOperator(doc)
	if err != nil {
		return nil, err
	}
	builtOperators.Put(doc, op)
	return op, nil
}

const maxFieldExpressions = 4096

var fieldExpressions struct {
	sync.RWMutex
	m map[string]*aggregations.Expression
}

func fieldExpression(s string) (*aggregations.Expression, error) {
	fieldExpressions.RLock()
	e, ok := fieldExpressions.m[s]
	fieldExpressions.RUnlock()
	if ok {
		return e, nil
	}
	e, err := aggregations.NewExpression(s, nil)
	if err != nil {
		return nil, err
	}
	fieldExpressions.Lock()
	if fieldExpressions.m == nil || len(fieldExpressions.m) >= maxFieldExpressions {
		fieldExpressions.m = make(map[string]*aggregations.Expression)
	}
	fieldExpressions.m[s] = e
	fieldExpressions.Unlock()
	return e, nil
}
