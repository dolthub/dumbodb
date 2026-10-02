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
	"github.com/dolthub/dumbodb/internal/types"
)

// WriteConcernDecision captures the subset of MongoDB's writeConcern options
// that affect DumboDB durability and acknowledgement.
type WriteConcernDecision struct {
	// Unsatisfiable is true when a single-node DumboDB cannot meet w.
	Unsatisfiable bool
	UnknownTag    bool
}

// DecideWriteConcern reports whether a single-node DumboDB can satisfy a raw
// writeConcern document. Every write is persisted before it is acknowledged,
// so j and w:0 do not change durability. A nil, missing, or malformed
// document is the MongoDB default {w: 1}.
func DecideWriteConcern(wc any) WriteConcernDecision {
	doc, ok := wc.(*types.Document)
	if !ok || doc == nil {
		return WriteConcernDecision{}
	}

	var unsatisfiable bool
	var unknownTag bool

	if v, err := doc.Get("w"); err == nil {
		switch w := v.(type) {
		case int32:
			unsatisfiable = w > 1
		case int64:
			unsatisfiable = w > 1
		case float64:
			unsatisfiable = w > 1
		case string:
			unknownTag = w != "majority"
		}
	}

	return WriteConcernDecision{Unsatisfiable: unsatisfiable, UnknownTag: unknownTag}
}
