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

package stages

import (
	"context"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// indexStatsStage implements the $indexStats aggregation stage.
// It returns one document per index in the collection with basic usage statistics.
type indexStatsStage struct {
	docs []*types.Document
}

// NewIndexStatsStage creates a new $indexStats stage.
// It pre-fetches the index list from the collection at construction time.
// host is the server's "hostname:port" included in each stats document.
func NewIndexStatsStage(stage *types.Document, ctx context.Context, c backends.Collection, host string) (aggregations.Stage, error) {
	res, err := c.ListIndexes(ctx, nil)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
			// A collection that does not exist has no indexes to report;
			// $indexStats returns an empty result set, matching MongoDB.
			return &indexStatsStage{docs: nil}, nil
		}
		return nil, err
	}

	now := time.Now().UTC()
	docs := make([]*types.Document, len(res.Indexes))

	for i, idx := range res.Indexes {
		keyDoc := must.NotFail(types.NewDocument())
		for _, kp := range idx.Key {
			switch {
			case kp.Text:
				keyDoc.Set(kp.Field, "text")
			case kp.Geo2DSphere:
				keyDoc.Set(kp.Field, "2dsphere")
			case kp.Geo2D:
				keyDoc.Set(kp.Field, "2d")
			case kp.Hashed:
				keyDoc.Set(kp.Field, "hashed")
			case kp.Descending:
				keyDoc.Set(kp.Field, int32(-1))
			default:
				keyDoc.Set(kp.Field, int32(1))
			}
		}

		doc := must.NotFail(types.NewDocument(
			"name", idx.Name,
			"key", keyDoc,
			"host", host,
			"accesses", must.NotFail(types.NewDocument(
				"ops", int64(0),
				"since", now,
			)),
		))
		docs[i] = doc
	}

	return &indexStatsStage{docs: docs}, nil
}

// Process implements aggregations.Stage.
// It discards the input iterator and returns one document per index.
func (s *indexStatsStage) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) {
	iter.Close()

	result := iterator.Values(iterator.ForSlice(s.docs))
	closer.Add(result)

	return result, nil
}

var _ aggregations.Stage = (*indexStatsStage)(nil)
