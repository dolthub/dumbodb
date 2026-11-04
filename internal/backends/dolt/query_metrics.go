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
	"context"
	"sync/atomic"

	"github.com/dolthub/dumbodb/internal/backends"
)

// queryExecutorCounters totals the index keys and documents read to answer
// queries, reported by serverStatus as metrics.queryExecutor.scanned and
// scannedObjects.
var queryExecutorCounters struct {
	keys atomic.Int64
	docs atomic.Int64
}

// countKeysExamined counts n index keys read, server-wide and for ctx's
// operation.
func countKeysExamined(ctx context.Context, n int) {
	queryExecutorCounters.keys.Add(int64(n))
	backends.AddKeysExamined(ctx, n)
}

// countDocsExamined counts n documents read, server-wide and for ctx's
// operation.
func countDocsExamined(ctx context.Context, n int) {
	queryExecutorCounters.docs.Add(int64(n))
	backends.AddDocsExamined(ctx, n)
}
