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
	"context"

	"github.com/dolthub/dumbodb/internal/backends"
)

// clientVisibleIndexes drops the _id index of a time-series collection, which
// mongod does not have.
func clientVisibleIndexes(ctx context.Context, db backends.Database, collection string, indexes []backends.IndexInfo) []backends.IndexInfo {
	info, err := lookupCollectionInfo(ctx, db, collection)
	if err != nil || info == nil || !info.IsTimeSeries {
		return indexes
	}
	visible := make([]backends.IndexInfo, 0, len(indexes))
	for _, idx := range indexes {
		if idx.Name != backends.DefaultIndexName {
			visible = append(visible, idx)
		}
	}
	return visible
}

// createTimeSeriesMetaTimeIndex creates the {<metaField>: 1, <timeField>: 1}
// index mongod builds for a new time-series collection with a metaField.
func createTimeSeriesMetaTimeIndex(ctx context.Context, db backends.Database, params *backends.CreateCollectionParams) error {
	if !params.IsTimeSeries || params.MetaField == "" {
		return nil
	}
	c, err := db.Collection(params.Name)
	if err != nil {
		return err
	}
	_, err = c.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{
		Name: params.MetaField + "_1_" + params.TimeField + "_1",
		Key:  []backends.IndexKeyPair{{Field: params.MetaField}, {Field: params.TimeField}},
	}}})
	return err
}

func hasIndexNamed(indexes []backends.IndexInfo, name string) bool {
	for _, idx := range indexes {
		if idx.Name == name {
			return true
		}
	}
	return false
}
