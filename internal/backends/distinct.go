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

import "context"

// DistinctParams configures DistinctScanner.DistinctScan.
type DistinctParams struct {
	// Key is the field name to extract distinct values for. Dot-paths are
	// not supported via this fast path; the handler falls back to a full
	// Query scan in that case.
	Key string
}

// DistinctResult carries the distinct values returned by DistinctScanner.
type DistinctResult struct {
	// Values is the deduplicated list of field values. Order is unspecified;
	// the handler sorts before returning to the client.
	Values []any
}

// DistinctScanner is an optional capability a backend may implement to compute
// `distinct` without a full collection scan  -- e.g. by walking a secondary
// index in sorted order and emitting one primary lookup per unique key.
//
// Implementations should return (nil, nil) when they cannot handle the request
// (no usable index, dot-path key, partial-index conditions that would drop
// rows, etc.). The handler then falls back to its Query-based path.
type DistinctScanner interface {
	DistinctScan(ctx context.Context, params *DistinctParams) (*DistinctResult, error)
}
