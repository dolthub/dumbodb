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
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// slowDecimalArray is an array of the decimals whose index keys cost the most
// to encode.
func slowDecimalArray(n int) *types.Array {
	h, l := must.NotFail(primitive.ParseDecimal128("1234567890123456789012345678901234E-233")).GetBytes()
	arr := types.MakeArray(n)
	for i := 0; i < n; i++ {
		arr.Append(types.Decimal128{H: h, L: l - uint64(i)})
	}
	return arr
}

// TestIndexEntriesStopAtDeadline: building the index entries of a large array
// of slow-to-encode values stops at the context's deadline, on insert and on
// update.
func TestIndexEntriesStopAtDeadline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "a", int32(0)))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_a", "a")
	coll := collAt(t, b, "testdb", "main", "items")

	const elements = 200_000
	const deadline = 100 * time.Millisecond
	const prompt = 2 * time.Second

	run := func(name string, write func(context.Context) error) {
		t.Run(name, func(t *testing.T) {
			wctx, cancel := context.WithTimeout(ctx, deadline)
			defer cancel()
			start := time.Now()
			err := write(wctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want context.DeadlineExceeded", err)
			}
			if elapsed := time.Since(start); elapsed > prompt {
				t.Fatalf("stopped after %v, want under %v", elapsed, prompt)
			}
		})
	}

	inserted := must.NotFail(types.NewDocument("_id", int32(2), "a", slowDecimalArray(elements)))
	updated := must.NotFail(types.NewDocument("_id", int32(1), "a", slowDecimalArray(elements)))
	run("insert", func(wctx context.Context) error {
		_, err := coll.InsertAll(wctx, &backends.InsertAllParams{Docs: []*types.Document{inserted}})
		return err
	})
	run("update", func(wctx context.Context) error {
		_, err := coll.UpdateAll(wctx, &backends.UpdateAllParams{Docs: []*types.Document{updated}})
		return err
	})
}
