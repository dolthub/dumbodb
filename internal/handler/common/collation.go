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
	"regexp"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
)

// Collation holds the parsed MongoDB collation options.
type Collation struct {
	Locale   string
	Strength int32
}

// CaseInsensitive reports whether the collation implies case-insensitive
// string comparison (strength 1 or 2).
func (c *Collation) CaseInsensitive() bool {
	return c != nil && c.Strength <= 2
}

// ParseCollation extracts collation parameters from a BSON document.
// It returns nil when doc is nil.
func ParseCollation(doc *types.Document) *Collation {
	if doc == nil {
		return nil
	}

	c := &Collation{
		Strength: 3, // MongoDB default
	}

	if v, err := doc.Get("locale"); err == nil {
		if s, ok := v.(string); ok {
			c.Locale = s
		}
	}

	if v, err := doc.Get("strength"); err == nil {
		switch s := v.(type) {
		case int32:
			c.Strength = s
		case int64:
			c.Strength = int32(s)
		case float64:
			c.Strength = int32(s)
		}
	}

	return c
}

// TransformFilterForCollation returns a deep copy of the filter with string
// equality values converted to case-insensitive Regex when the collation is
// case-insensitive. All other filter expressions are left unchanged.
//
// Only top-level simple field equality values (e.g. {field: "value"}) and
// values nested under $and/$or/$nor are transformed.
func TransformFilterForCollation(filter *types.Document, c *Collation) *types.Document {
	if !c.CaseInsensitive() || filter == nil {
		return filter
	}

	result := filter.DeepCopy()
	transformCollationDoc(result)

	return result
}

// transformCollationDoc recursively replaces string equality values with
// case-insensitive regexes inside a filter document.
func transformCollationDoc(doc *types.Document) {
	for _, key := range doc.Keys() {
		val, err := doc.Get(key)
		if err != nil {
			continue
		}

		switch key {
		case "$and", "$or", "$nor":
			// Recurse into array of filter documents.
			arr, ok := val.(*types.Array)
			if !ok {
				continue
			}

			for i := 0; i < arr.Len(); i++ {
				elem, err := arr.Get(i)
				if err != nil {
					continue
				}

				subDoc, ok := elem.(*types.Document)
				if !ok {
					continue
				}

				transformCollationDoc(subDoc)
			}

		default:
			if strings.HasPrefix(key, "$") {
				// Operator key: skip (operators like $expr, $text, etc.)
				continue
			}

			// Simple field equality: {field: "value"} → {field: /^value$/i}
			if s, ok := val.(string); ok {
				doc.Set(key, stringToInsensitiveRegex(s))
			}
		}
	}
}

// stringToInsensitiveRegex converts a plain string into a case-insensitive
// Regex that matches exactly that string.
func stringToInsensitiveRegex(s string) types.Regex {
	return types.Regex{
		Pattern: "^" + regexp.QuoteMeta(s) + "$",
		Options: "i",
	}
}

// lessFuncCaseInsensitive returns a sort comparator that compares string fields
// case-insensitively. Non-string values fall back to standard comparison.
func lessFuncCaseInsensitive(sortPath types.Path, sortType types.SortType) func(a, b *types.Document) bool {
	return func(a, b *types.Document) bool {
		aField, err := a.GetByPath(sortPath)
		if err != nil {
			aField = types.Null
		}

		bField, err := b.GetByPath(sortPath)
		if err != nil {
			bField = types.Null
		}

		// Case-insensitive string comparison.
		aStr, aIsStr := aField.(string)
		bStr, bIsStr := bField.(string)

		if aIsStr && bIsStr {
			aLow := strings.ToLower(aStr)
			bLow := strings.ToLower(bStr)

			if aLow == bLow {
				return false
			}

			if sortType == types.Ascending {
				return aLow < bLow
			}

			return aLow > bLow
		}

		// Fall back to standard comparison for non-string values.
		result := types.CompareOrderForSort(aField, bField, sortType)

		return result == types.Less
	}
}
