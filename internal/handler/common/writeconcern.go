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
// that affect DumboDB durability. Everything else (wtimeout, majority, custom
// tags) is accepted but ignored because DumboDB is a single-node store.
type WriteConcernDecision struct {
	// SkipDurableSync is true when the client opted out of a synchronous journal
	// flush for this write. This is the union of two cases:
	//   - writeConcern.j == false (explicit opt-out of journal fsync)
	//   - writeConcern.w == 0 (fire-and-forget: no durability guarantee at all)
	// In either case DumboDB is free to acknowledge the write before the NBS
	// journal is fsync'd  -- a background flusher makes it durable later.
	SkipDurableSync bool
}

// DecideWriteConcern extracts the durability decision from a raw writeConcern
// document. A nil or missing document maps to the MongoDB default of
// {w: 1, j: true (implicit)}, which keeps the historical fsync-every-write
// behavior. Unrecognized or malformed writeConcern shapes are treated as the
// default  -- we never error out on writeConcern because a single-node store
// has no way to meaningfully reject the cluster-shaped concerns (majority, w>1).
func DecideWriteConcern(wc any) WriteConcernDecision {
	doc, ok := wc.(*types.Document)
	if !ok || doc == nil {
		return WriteConcernDecision{}
	}

	var skip bool

	if v, err := doc.Get("w"); err == nil {
		switch w := v.(type) {
		case int32:
			if w == 0 {
				skip = true
			}
		case int64:
			if w == 0 {
				skip = true
			}
		case float64:
			if w == 0 {
				skip = true
			}
		}
	}

	if v, err := doc.Get("j"); err == nil {
		if b, ok := v.(bool); ok && !b {
			skip = true
		}
	}

	return WriteConcernDecision{SkipDurableSync: skip}
}
