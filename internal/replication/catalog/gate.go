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

package catalog

import (
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

type UnsupportedFeatureError struct {
	Database   string
	Collection string
	Index      string
	Option     string
	Reason     string
}

func (e *UnsupportedFeatureError) Error() string {
	location := e.Database + "." + e.Collection
	if e.Index != "" {
		location += " index " + e.Index
	}
	return fmt.Sprintf("replication rejected %s option %q: %s", location, e.Option, e.Reason)
}

type CollectionPlan struct {
	Create  backends.CreateCollectionParams
	Indexes []backends.IndexInfo
}

func PreflightCollection(database, collection string, options *types.Document, indexDocuments []*types.Document) (CollectionPlan, error) {
	plan := CollectionPlan{Create: backends.CreateCollectionParams{Name: collection}}
	if options != nil {
		if err := applyCollectionOptions(database, collection, options, &plan.Create); err != nil {
			return CollectionPlan{}, err
		}
	}
	indexes, err := PreflightIndexes(database, collection, indexDocuments)
	if err != nil {
		return CollectionPlan{}, err
	}
	plan.Indexes = indexes
	return plan, nil
}

func PreflightIndexes(database, collection string, documents []*types.Document) ([]backends.IndexInfo, error) {
	indexes := make([]backends.IndexInfo, 0, len(documents))
	for _, document := range documents {
		index, err := preflightIndex(database, collection, document)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func applyCollectionOptions(database, collection string, options *types.Document, params *backends.CreateCollectionParams) error {
	iter := options.Iterator()
	defer iter.Close()
	for {
		option, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil
		}
		if err != nil {
			return err
		}
		switch option {
		case "validator":
			params.Validator, _ = value.(*types.Document)
			if params.Validator == nil {
				return invalidOption(database, collection, "", option, "must be a document")
			}
		case "validationLevel":
			params.ValidationLevel, _ = value.(string)
			if params.ValidationLevel != "strict" && params.ValidationLevel != "moderate" && params.ValidationLevel != "off" {
				return invalidOption(database, collection, "", option, "must be strict, moderate, or off")
			}
		case "validationAction":
			params.ValidationAction, _ = value.(string)
			if params.ValidationAction != "error" && params.ValidationAction != "warn" {
				return invalidOption(database, collection, "", option, "must be error or warn")
			}
		case "collation":
			params.Collation, _ = value.(*types.Document)
			if params.Collation == nil {
				return invalidOption(database, collection, "", option, "must be a document")
			}
		case "viewOn":
			params.ViewOn, _ = value.(string)
			if params.ViewOn == "" {
				return invalidOption(database, collection, "", option, "must be a non-empty string")
			}
		case "pipeline":
			params.ViewPipeline, _ = value.(*types.Array)
			if params.ViewPipeline == nil {
				return invalidOption(database, collection, "", option, "must be an array")
			}
		case "timeseries":
			timeSeries, ok := value.(*types.Document)
			if !ok {
				return invalidOption(database, collection, "", option, "must be a document")
			}
			if err := applyTimeSeries(database, collection, timeSeries, params); err != nil {
				return err
			}
		case "capped", "size", "max":
			return invalidOption(database, collection, "", option, "capped collection semantics are unsupported")
		case "expireAfterSeconds":
			return invalidOption(database, collection, "", option, "TTL conflicts with historical storage")
		case "clusteredIndex":
			return invalidOption(database, collection, "", option, "clustered collections are unsupported")
		case "changeStreamPreAndPostImages":
			return invalidOption(database, collection, "", option, "change-stream pre-image retention is unsupported")
		default:
			return invalidOption(database, collection, "", option, "unknown collection option would be silently degraded")
		}
	}
}

func applyTimeSeries(database, collection string, document *types.Document, params *backends.CreateCollectionParams) error {
	params.IsTimeSeries = true
	iter := document.Iterator()
	defer iter.Close()
	for {
		option, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			if params.TimeField == "" {
				return invalidOption(database, collection, "", "timeseries.timeField", "is required")
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch option {
		case "timeField":
			params.TimeField, _ = value.(string)
		case "metaField":
			params.MetaField, _ = value.(string)
		case "granularity":
			params.Granularity, _ = value.(string)
		case "bucketMaxSpanSeconds", "bucketRoundingSeconds":
			return invalidOption(database, collection, "", "timeseries."+option, "custom bucket spans are not represented")
		default:
			return invalidOption(database, collection, "", "timeseries."+option, "unknown time-series option")
		}
	}
}

func preflightIndex(database, collection string, document *types.Document) (backends.IndexInfo, error) {
	nameValue, _ := document.Get("name")
	name, _ := nameValue.(string)
	if name == "" {
		return backends.IndexInfo{}, invalidOption(database, collection, "", "name", "index name is required")
	}
	keyValue, _ := document.Get("key")
	keyDocument, ok := keyValue.(*types.Document)
	if !ok || keyDocument.Len() == 0 {
		return backends.IndexInfo{}, invalidOption(database, collection, name, "key", "non-empty key document is required")
	}
	index := backends.IndexInfo{Name: name}
	keyIter := keyDocument.Iterator()
	defer keyIter.Close()
	for {
		field, direction, err := keyIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return backends.IndexInfo{}, err
		}
		key := backends.IndexKeyPair{Field: field}
		switch direction {
		case int32(1), int64(1), float64(1):
		case int32(-1), int64(-1), float64(-1):
			key.Descending = true
		case "hashed":
			key.Hashed = true
		case "text", "2d", "2dsphere":
			return backends.IndexInfo{}, invalidOption(database, collection, name, "key."+field, fmt.Sprintf("index type %q is unsupported", direction))
		default:
			return backends.IndexInfo{}, invalidOption(database, collection, name, "key."+field, "unsupported index key type")
		}
		if field == "$**" {
			return backends.IndexInfo{}, invalidOption(database, collection, name, "key.$**", "wildcard indexes are unsupported")
		}
		index.Key = append(index.Key, key)
	}
	iter := document.Iterator()
	defer iter.Close()
	for {
		option, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			if index.Unique {
				for _, key := range index.Key {
					if key.Hashed {
						return backends.IndexInfo{}, invalidOption(database, collection, name, "unique", "hashed indexes cannot enforce uniqueness")
					}
				}
			}
			return index, nil
		}
		if err != nil {
			return backends.IndexInfo{}, err
		}
		switch option {
		case "v", "key", "name", "ns", "background":
		case "unique":
			index.Unique, ok = value.(bool)
			if !ok {
				return backends.IndexInfo{}, invalidOption(database, collection, name, option, "must be boolean")
			}
		case "sparse":
			index.Sparse, ok = value.(bool)
			if !ok {
				return backends.IndexInfo{}, invalidOption(database, collection, name, option, "must be boolean")
			}
		case "partialFilterExpression":
			index.PartialFilterExpression, ok = value.(*types.Document)
			if !ok {
				return backends.IndexInfo{}, invalidOption(database, collection, name, option, "must be a document")
			}
			filter := index.PartialFilterExpression
			index.MatchesPartialFilter = func(document *types.Document) (bool, error) {
				return backends.MatchPartialFilter(document, filter)
			}
		case "collation":
			collation, ok := value.(*types.Document)
			if !ok || !simpleCollation(collation) {
				return backends.IndexInfo{}, invalidOption(database, collection, name, option, "non-simple index collation is not enforced")
			}
			index.Collation = collation
		case "expireAfterSeconds":
			return backends.IndexInfo{}, invalidOption(database, collection, name, option, "TTL conflicts with historical storage")
		case "hidden":
			if hidden, _ := value.(bool); hidden {
				return backends.IndexInfo{}, invalidOption(database, collection, name, option, "hidden planner behavior is not enforced")
			}
		case "storageEngine", "weights", "default_language", "language_override", "textIndexVersion", "2dsphereIndexVersion", "bits", "min", "max", "bucketSize", "wildcardProjection", "buildUUID":
			return backends.IndexInfo{}, invalidOption(database, collection, name, option, "option is not represented or enforced")
		default:
			return backends.IndexInfo{}, invalidOption(database, collection, name, option, "unknown index option would be silently degraded")
		}
	}
}

func simpleCollation(document *types.Document) bool {
	if document == nil {
		return false
	}
	locale, _ := document.Get("locale")
	return locale == "simple"
}

func invalidOption(database, collection, index, option, reason string) error {
	return &UnsupportedFeatureError{Database: database, Collection: collection, Index: index, Option: option, Reason: reason}
}
