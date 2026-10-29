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
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ValidatePipelineMetadata returns MongoDB's error for a pipeline that asks
// for text score or geoNear metadata no stage provides: $text in a leading
// $match supplies the text score, and a leading $geoNear the geoNear
// fields. Sub-pipelines of $lookup, $graphLookup and $unionWith have their
// own metadata and are not inspected.
func ValidatePipelineMetadata(stages []any) error {
	var first *types.Document
	if len(stages) > 0 {
		first, _ = stages[0].(*types.Document)
	}

	used := map[string]bool{}
	for _, stage := range stages {
		doc, ok := stage.(*types.Document)
		if !ok || doc.Len() == 0 {
			continue
		}
		switch doc.Command() {
		case "$lookup", "$graphLookup", "$unionWith":
			continue
		}
		collectMetaNames(doc, used)
	}

	hasText := first != nil && first.Command() == "$match" && filterHasText(must.NotFail(first.Get("$match")))
	hasGeoNear := first != nil && first.Command() == "$geoNear"
	return metadataUnavailableError(used, hasText, hasGeoNear)
}

// ValidateFindMetadata is ValidatePipelineMetadata for a find command's
// filter, projection and sort.
func ValidateFindMetadata(filter, projection, sort *types.Document) error {
	used := map[string]bool{}
	for _, doc := range []*types.Document{projection, sort} {
		if doc != nil {
			collectMetaNames(doc, used)
		}
	}
	if used["sortKey"] && sort.Len() == 0 {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			"$meta: \"sortKey\" requires the query to have a sort", "$meta")
	}
	return metadataUnavailableError(used, filter != nil && filterHasText(filter), false)
}

func metadataUnavailableError(used map[string]bool, hasText, hasGeoNear bool) error {
	for _, name := range []string{"textScore", "geoNearDistance", "geoNearPoint"} {
		if !used[name] || (name == "textScore" && hasText) || (name != "textScore" && hasGeoNear) {
			continue
		}
		what := "text score"
		if name != "textScore" {
			what = name
		}
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMetadataUnavailable,
			fmt.Sprintf("query requires %s metadata, but it is not available", what), "$meta")
	}
	return nil
}

// collectMetaNames records the string operand of every $meta in v.
func collectMetaNames(v any, used map[string]bool) {
	switch v := v.(type) {
	case *types.Document:
		for _, k := range v.Keys() {
			value := must.NotFail(v.Get(k))
			if name, ok := value.(string); ok && k == "$meta" {
				used[name] = true
				continue
			}
			collectMetaNames(value, used)
		}
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			collectMetaNames(must.NotFail(v.Get(i)), used)
		}
	}
}

// filterHasText reports whether a query filter has a top-level $text,
// possibly under $and or $or.
func filterHasText(filter any) bool {
	doc, ok := filter.(*types.Document)
	if !ok {
		return false
	}
	for _, k := range doc.Keys() {
		switch k {
		case "$text":
			return true
		case "$and", "$or":
			if branches, ok := must.NotFail(doc.Get(k)).(*types.Array); ok {
				for i := 0; i < branches.Len(); i++ {
					if filterHasText(must.NotFail(branches.Get(i))) {
						return true
					}
				}
			}
		}
	}
	return false
}
