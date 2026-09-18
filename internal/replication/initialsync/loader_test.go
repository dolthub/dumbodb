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

package initialsync

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestBoundedCollectionLoaderFlushesAtDocumentLimit(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}); err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("items")
	if err != nil {
		t.Fatal(err)
	}
	loader, err := NewBoundedCollectionLoader(collection, LoaderLimits{Documents: 2, Bytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for id := int32(0); id < 5; id++ {
		if err := loader.Add(ctx, must.NotFail(types.NewDocument("_id", id, "payload", "bounded"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := loader.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	stats := loader.Stats()
	if stats.Documents != 5 || stats.Batches != 3 || stats.PeakBufferedDocs != 2 || stats.PeakBufferedBytes > 1024 {
		t.Fatalf("loader stats = %+v", stats)
	}
	result, err := collection.Query(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Iter.Close()
	count := 0
	for {
		_, _, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 5 {
		t.Fatalf("loaded document count = %d", count)
	}
}

func TestBoundedCollectionLoaderFlushesBeforeByteLimit(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("items")
	if err != nil {
		t.Fatal(err)
	}
	first := must.NotFail(types.NewDocument("_id", int32(1), "payload", "aaaaaaaaaaaaaaaa"))
	loader, err := NewBoundedCollectionLoader(collection, LoaderLimits{Documents: 100, Bytes: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.Add(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := loader.Add(ctx, must.NotFail(types.NewDocument("_id", int32(2), "payload", "bbbbbbbbbbbbbbbb"))); err != nil {
		t.Fatal(err)
	}
	if err := loader.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	stats := loader.Stats()
	if stats.Documents != 2 || stats.Batches != 2 || stats.PeakBufferedDocs != 1 || stats.PeakBufferedBytes > 50 {
		t.Fatalf("loader stats = %+v", stats)
	}
}
