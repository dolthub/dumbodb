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

	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// countShortcutInfo describes a pipeline that is equivalent to MongoDB's
// CountDocuments aggregation. The handler can satisfy it via the backend's
// O(1) Count() rather than a full scan.
type countShortcutInfo struct {
	outField string // name of the count output field, e.g. "n"
	idValue  any    // literal value used for $group _id (echoed back to the client)
	skip     int64  // number of documents to skip; 0 if absent
	limit    int64  // maximum documents to return; -1 if absent
}

// tryCountAggregateShortcut detects the exact aggregate pipeline shape that
// the MongoDB drivers emit for CountDocuments with an empty filter:
//
//	[{$match: <empty>}, optional {$skip: N}, optional {$limit: N},
//	 {$group: {_id: <const>, <field>: {$sum: 1}}}]
//
// Returns ok=true and a countShortcutInfo when the pipeline matches and the
// $match filter is empty (so the result is the full collection size adjusted
// by skip/limit). Pipelines with a non-empty $match still need the scan path.
func tryCountAggregateShortcut(stages []any) (countShortcutInfo, bool) {
	var info countShortcutInfo
	info.limit = -1

	if len(stages) == 0 {
		return info, false
	}

	seenGroup := false

	for i, s := range stages {
		if seenGroup {
			return countShortcutInfo{}, false
		}

		d, ok := s.(*types.Document)
		if !ok {
			return countShortcutInfo{}, false
		}

		switch d.Command() {
		case "$match":
			if i != 0 {
				return countShortcutInfo{}, false
			}
			v, _ := d.Get("$match")
			md, mok := v.(*types.Document)
			if !mok || md.Len() != 0 {
				return countShortcutInfo{}, false
			}

		case "$skip":
			v, _ := d.Get("$skip")
			n, err := handlerparams.GetWholeNumberParam(v)
			if err != nil || n < 0 {
				return countShortcutInfo{}, false
			}
			info.skip = n

		case "$limit":
			v, _ := d.Get("$limit")
			n, err := handlerparams.GetWholeNumberParam(v)
			if err != nil || n < 0 {
				return countShortcutInfo{}, false
			}
			info.limit = n

		case "$group":
			spec, _ := d.Get("$group")
			sd, sok := spec.(*types.Document)
			if !sok || sd.Len() != 2 {
				return countShortcutInfo{}, false
			}

			idVal, idErr := sd.Get("_id")
			if idErr != nil || !isCountGroupIDLiteral(idVal) {
				return countShortcutInfo{}, false
			}

			var outField string
			for _, k := range sd.Keys() {
				if k != "_id" {
					outField = k
					break
				}
			}
			if outField == "" {
				return countShortcutInfo{}, false
			}

			accV, _ := sd.Get(outField)
			accD, aok := accV.(*types.Document)
			if !aok || accD.Len() != 1 {
				return countShortcutInfo{}, false
			}

			sumV, sErr := accD.Get("$sum")
			if sErr != nil {
				return countShortcutInfo{}, false
			}

			sumN, err := handlerparams.GetWholeNumberParam(sumV)
			if err != nil || sumN != 1 {
				return countShortcutInfo{}, false
			}

			info.outField = outField
			info.idValue = idVal
			seenGroup = true

		default:
			return countShortcutInfo{}, false
		}
	}

	if !seenGroup {
		return countShortcutInfo{}, false
	}

	return info, true
}

// isCountGroupIDLiteral reports whether v is a literal scalar suitable as a
// constant $group key (so every document maps to the same group). Documents,
// arrays, and "$"-prefixed strings (field references / expressions) are
// rejected; everything else is treated as a constant.
func isCountGroupIDLiteral(v any) bool {
	switch x := v.(type) {
	case *types.Document, *types.Array:
		return false
	case string:
		return !strings.HasPrefix(x, "$")
	default:
		return true
	}
}
