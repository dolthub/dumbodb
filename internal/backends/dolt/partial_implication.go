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

package dolt

import (
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// conditionExcludesMissing reports whether the filter condition fv can never
// match a document missing the field, so it implies a partial index's
// {$exists: true}: a non-null equality, a comparison against a non-null
// scalar, $exists: true, or $in without null.
func conditionExcludesMissing(fv any) bool {
	if _, ok := partialFilterScalar(fv); ok {
		return true
	}
	ops, isOp := fv.(*types.Document)
	if !isOp {
		return false
	}
	for _, op := range ops.Keys() {
		v := must.NotFail(ops.Get(op))
		switch op {
		case "$eq", "$gt", "$gte", "$lt", "$lte":
			if _, ok := partialFilterScalar(v); ok {
				return true
			}
		case "$exists":
			if existsTruthy(v) {
				return true
			}
		case "$in":
			if values, isArr := v.(*types.Array); isArr && values.Len() > 0 && !values.Contains(types.Null) {
				return true
			}
		}
	}
	return false
}
