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
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// init wires FilterDocumentColl as the global partial-index filter matcher (so
// the backend can rebuild IndexInfo.MatchesPartialFilter closures after loading
// indexes from disk) and FilterDocument as the global document-validator
// matcher (so the backend can enforce validators during a merge), without
// importing handler/common.
func init() {
	backends.RegisterPartialFilterMatcher(FilterDocumentColl)
	backends.RegisterFilterCanonicalizer(canonicalFilterCopy)
	backends.RegisterPartialFilterCompiler(compilePartialFilter)
	backends.RegisterValidatorMatcher(FilterDocument)
}

func canonicalFilterCopy(filter *types.Document) *types.Document {
	canonical := filter.DeepCopy()
	CanonicalizeFilter(canonical)
	return canonical
}

// compilePartialFilter returns the membership predicate of partial filter
// pfe. A document being written may not have its keys sorted yet, so when pfe
// compares embedded documents the document is matched in sorted-key form,
// as it is stored: all but an _id, which keeps its field order.
func compilePartialFilter(pfe *types.Document, cmp *collation.Comparator) func(doc *types.Document) (bool, error) {
	canonical := canonicalFilterCopy(pfe)
	sortDoc := comparesEmbeddedDocuments(canonical)
	return func(doc *types.Document) (bool, error) {
		if sortDoc {
			doc = sortedKeysExceptID(doc)
		}
		return FilterDocumentColl(doc, canonical, cmp)
	}
}

// comparesEmbeddedDocuments reports whether a filter compares a field with a
// document value, at any depth.
func comparesEmbeddedDocuments(v any) bool {
	switch v := v.(type) {
	case *types.Document:
		for _, key := range v.Keys() {
			value := must.NotFail(v.Get(key))
			if d, ok := value.(*types.Document); ok && !strings.HasPrefix(key, "$") && !isOperatorDocument(d) {
				return true
			}
			switch key {
			case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte":
				if _, ok := value.(*types.Document); ok {
					return true
				}
			}
			if comparesEmbeddedDocuments(value) {
				return true
			}
		}
	case *types.Array:
		for i := range v.Len() {
			elem := must.NotFail(v.Get(i))
			if _, ok := elem.(*types.Document); ok {
				return true
			}
			if comparesEmbeddedDocuments(elem) {
				return true
			}
		}
	}
	return false
}

func sortedKeysExceptID(doc *types.Document) *types.Document {
	sorted := sortedKeys(doc).(*types.Document)
	if id, err := doc.Get("_id"); err == nil {
		sorted.Set("_id", id)
	}
	return sorted
}
