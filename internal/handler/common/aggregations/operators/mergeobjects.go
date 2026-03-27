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
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/must"
)

// mergeObjects implements the $mergeObjects expression operator.
//
// $mergeObjects combines multiple documents into one. Later documents override
// earlier ones for duplicate keys. Null or missing values are ignored.
// Arguments can be document literals, field path expressions, or $$ROOT.
type mergeObjects struct {
	args []any
}

// newMergeObjects creates a new $mergeObjects operator.
// Each argument is an expression that evaluates to a document (or null).
func newMergeObjects(args ...any) (Operator, error) {
	return &mergeObjects{args: args}, nil
}

// Process implements the Operator interface.
// It evaluates each argument against doc and merges the resulting documents.
func (m *mergeObjects) Process(doc *types.Document) (any, error) {
	result, err := types.NewDocument()
	if err != nil {
		return nil, err
	}

	for _, arg := range m.args {
		val, err := evalArgValueWithRoot(arg, doc)
		if err != nil {
			// null/missing arguments are ignored
			continue
		}

		src, ok := val.(*types.Document)
		if !ok {
			// null or non-document values are ignored
			continue
		}

		for _, k := range src.Keys() {
			result.Set(k, must.NotFail(src.Get(k)))
		}
	}

	return result, nil
}

// evalArgValueWithRoot is like evalArgValue but also handles the $$ROOT system
// variable by returning the current document.
func evalArgValueWithRoot(arg any, doc *types.Document) (any, error) {
	if s, ok := arg.(string); ok && s == "$$ROOT" {
		return doc.DeepCopy(), nil
	}

	return evalArgValue(arg, doc)
}

// check interfaces
var (
	_ Operator = (*mergeObjects)(nil)
)
