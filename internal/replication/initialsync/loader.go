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
	"fmt"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

type LoaderLimits struct {
	Documents int
	Bytes     int64
}

type LoaderStats struct {
	Documents         int64
	Batches           int64
	PeakBufferedDocs  int
	PeakBufferedBytes int64
}

type BoundedCollectionLoader struct {
	collection    backends.Collection
	limits        LoaderLimits
	documents     []*types.Document
	bufferedBytes int64
	stats         LoaderStats
}

func NewBoundedCollectionLoader(collection backends.Collection, limits LoaderLimits) (*BoundedCollectionLoader, error) {
	if collection == nil || limits.Documents <= 0 || limits.Bytes <= 0 {
		return nil, errors.New("collection loader requires a collection and positive document/byte limits")
	}
	return &BoundedCollectionLoader{collection: collection, limits: limits}, nil
}

func (l *BoundedCollectionLoader) Add(ctx context.Context, document *types.Document) error {
	if document == nil {
		return errors.New("cannot load a nil document")
	}
	raw, err := bson.FromDocumentRaw(document)
	if err != nil {
		return fmt.Errorf("encoding cloned document: %w", err)
	}
	size := int64(len(raw))
	if size > l.limits.Bytes {
		return fmt.Errorf("cloned document size %d exceeds loader byte limit %d", size, l.limits.Bytes)
	}
	if len(l.documents) > 0 && (len(l.documents) >= l.limits.Documents || l.bufferedBytes+size > l.limits.Bytes) {
		if err := l.Flush(ctx); err != nil {
			return err
		}
	}
	l.documents = append(l.documents, document)
	l.bufferedBytes += size
	l.stats.PeakBufferedDocs = max(l.stats.PeakBufferedDocs, len(l.documents))
	l.stats.PeakBufferedBytes = max(l.stats.PeakBufferedBytes, l.bufferedBytes)
	if len(l.documents) >= l.limits.Documents || l.bufferedBytes >= l.limits.Bytes {
		return l.Flush(ctx)
	}
	return nil
}

func (l *BoundedCollectionLoader) Flush(ctx context.Context) error {
	if len(l.documents) == 0 {
		return nil
	}
	documentCount := len(l.documents)
	if bulkLoader, ok := l.collection.(backends.InitialSyncCollection); ok {
		if err := bulkLoader.BulkLoadInitialSync(ctx, l.documents); err != nil {
			return fmt.Errorf("bulk-loading cloned document batch: %w", err)
		}
	} else if _, err := l.collection.InsertAll(ctx, &backends.InsertAllParams{Docs: l.documents}); err != nil {
		return fmt.Errorf("loading cloned document batch: %w", err)
	}
	l.stats.Documents += int64(documentCount)
	l.stats.Batches++
	l.documents = nil
	l.bufferedBytes = 0
	return nil
}

func (l *BoundedCollectionLoader) Stats() LoaderStats {
	return l.stats
}
