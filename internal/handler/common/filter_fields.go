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

package common

import (
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
)

// FilterRootFields returns the top-level fields filter reads. It returns false
// when filter may read fields it does not name, as $expr and $where can.
func FilterRootFields(filter *types.Document) ([]string, bool) {
	var roots []string
	if !appendFilterRootFields(filter, &roots) {
		return nil, false
	}
	return roots, true
}

func appendFilterRootFields(filter *types.Document, roots *[]string) bool {
	if filter == nil {
		return true
	}
	keys, values := filter.Keys(), filter.Values()
	for i, key := range keys {
		switch key {
		case "$and", "$or", "$nor":
			clauses, ok := values[i].(*types.Array)
			if !ok {
				return false
			}
			for j := 0; j < clauses.Len(); j++ {
				clause, _ := clauses.Get(j)
				doc, ok := clause.(*types.Document)
				if !ok || !appendFilterRootFields(doc, roots) {
					return false
				}
			}
		case "$comment":
		default:
			if strings.HasPrefix(key, "$") {
				return false
			}
			root, _, _ := strings.Cut(key, ".")
			if !slices.Contains(*roots, root) {
				*roots = append(*roots, root)
			}
		}
	}
	return true
}
