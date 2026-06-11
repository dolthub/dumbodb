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

import "github.com/dolthub/dumbodb/internal/types"

// HintIsNatural reports whether hint is the {"$natural": <int>} pattern.
// MongoDB treats this as "scan storage in natural order; do not pick an index
// regardless of what filter/sort suggests."
func HintIsNatural(hint any) bool {
	doc, ok := hint.(*types.Document)
	if !ok || doc == nil || doc.Len() != 1 {
		return false
	}

	return doc.Keys()[0] == "$natural"
}

// HintRequiresExistingIndex reports whether hint names a specific index that
// must exist. A nil, empty, or $natural hint imposes no such requirement.
func HintRequiresExistingIndex(hint any) bool {
	switch h := hint.(type) {
	case nil:
		return false
	case string:
		return h != ""
	case *types.Document:
		return h != nil && h.Len() > 0 && !HintIsNatural(h)
	default:
		return false
	}
}

// MatchHintedIndex resolves a hint value (either a name string or a key-pattern
// document) to a matching index name, or returns "" if no index matches.
func MatchHintedIndex(hint any, idxInfos []IndexInfo) string {
	if hint == nil {
		return ""
	}

	switch h := hint.(type) {
	case string:
		for _, idx := range idxInfos {
			if idx.Name == h {
				return idx.Name
			}
		}
	case *types.Document:
		if h == nil || h.Len() == 0 {
			return ""
		}

		hintKeys := h.Keys()
		for _, idx := range idxInfos {
			if len(idx.Key) != len(hintKeys) {
				continue
			}

			match := true
			for i, k := range hintKeys {
				if idx.Key[i].Field != k {
					match = false
					break
				}
			}

			if match {
				return idx.Name
			}
		}
	}

	return ""
}
