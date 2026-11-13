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

package backends

import (
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// PushableSort reports whether a query hands sort to the backend, which may
// serve it by reading an index in order: any one-key sort, or a sort on
// several fields all in the same direction. An index stores every key
// ascending, so a mixed-direction sort is applied in memory.
func PushableSort(sort *types.Document) bool {
	switch {
	case sort.Len() == 0:
		return false
	case sort.Len() == 1:
		return true
	}
	var first int
	for i, k := range sort.Keys() {
		if strings.HasPrefix(k, "$") {
			return false
		}
		dir := sortDirection(must.NotFail(sort.Get(k)))
		if dir == 0 || (i > 0 && dir != first) {
			return false
		}
		first = dir
	}
	return true
}

// sortDirection is 1 or -1 for a sort value, 0 for any other value.
func sortDirection(v any) int {
	var f float64
	switch x := v.(type) {
	case int32:
		f = float64(x)
	case int64:
		f = float64(x)
	case float64:
		f = x
	default:
		return 0
	}
	switch f {
	case 1:
		return 1
	case -1:
		return -1
	}
	return 0
}
