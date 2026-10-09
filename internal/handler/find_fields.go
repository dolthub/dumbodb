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

package handler

import (
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
)

// findRootFields returns the top-level fields a find reads when its projection
// includes fields. It returns false for exclusion projections and for options
// that can read fields they do not name.
func findRootFields(params *common.FindParams) ([]string, bool) {
	if params.Projection.Len() == 0 || params.ReturnKey || params.Min != nil || params.Max != nil {
		return nil, false
	}

	roots, ok := common.FilterRootFields(params.Filter)
	if !ok {
		return nil, false
	}
	roots = appendRoot(roots, "_id")

	if params.Sort != nil {
		keys := params.Sort.Keys()
		for i, v := range params.Sort.Values() {
			if _, isDoc := v.(*types.Document); isDoc {
				return nil, false
			}
			if keys[i] != "$natural" {
				roots = appendRoot(roots, keys[i])
			}
		}
	}

	included, ok := appendProjectionRootFields(params.Projection, &roots, true)
	if !ok || !included {
		return nil, false
	}
	return roots, true
}

// appendProjectionRootFields adds the fields an inclusion projection reads and
// reports whether it includes anything. It returns false for exclusions.
func appendProjectionRootFields(projection *types.Document, roots *[]string, top bool) (included, ok bool) {
	keys := projection.Keys()
	for i, v := range projection.Values() {
		key := keys[i]
		switch v := v.(type) {
		case bool, int32, int64, float64:
			if !isTruthy(v) {
				if top && key == "_id" {
					continue
				}
				return false, false
			}
			*roots = appendRoot(*roots, key)
		case *types.Document:
			if op := v.Command(); strings.HasPrefix(op, "$") {
				switch op {
				case "$slice", "$elemMatch":
					*roots = appendRoot(*roots, key)
				case "$meta":
					return false, false
				default:
					if !appendExprRootFields(v, roots) {
						return false, false
					}
				}
			} else {
				*roots = appendRoot(*roots, key)
				if _, ok := appendProjectionRootFields(v, roots, false); !ok {
					return false, false
				}
			}
		case string, *types.Array:
			if !appendExprRootFields(v, roots) {
				return false, false
			}
		default:
			return false, false
		}
		included = true
	}
	return included, true
}

func isTruthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	}
	return false
}
