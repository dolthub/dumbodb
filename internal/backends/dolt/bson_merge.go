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
	"reflect"
	"sort"

	"github.com/dolthub/dumbodb/internal/types"
)

// mergeBSONDoc recursively merges existing documents. Arrays remain atomic.
// A conflict returns (nil, true).
func mergeBSONDoc(base, left, right *types.Document) (*types.Document, bool) {
	keys := mergeDocumentKeys(base, left, right)
	out := types.MakeDocument(len(keys))
	for _, k := range keys {
		bVal, bOK := getOrNil(base, k)
		lVal, lOK := getOrNil(left, k)
		rVal, rOK := getOrNil(right, k)
		switch {
		case lOK && !bOK && !rOK:
			out.Set(k, lVal)
		case rOK && !bOK && !lOK:
			out.Set(k, rVal)
		case lOK && rOK && !bOK:
			if reflect.DeepEqual(lVal, rVal) {
				out.Set(k, lVal)
				continue
			}
			return nil, true
		case bOK && lOK && !rOK:
			// Modify against delete: the sides disagree about whether the
			// value should exist. An unmodified survivor means a plain
			// delete, which wins.
			if !reflect.DeepEqual(lVal, bVal) {
				return nil, true
			}
		case bOK && rOK && !lOK:
			if !reflect.DeepEqual(rVal, bVal) {
				return nil, true
			}
		case bOK && lOK && rOK:
			leftUnchanged := reflect.DeepEqual(lVal, bVal)
			rightUnchanged := reflect.DeepEqual(rVal, bVal)
			sameMod := reflect.DeepEqual(lVal, rVal)
			switch {
			case leftUnchanged && rightUnchanged:
				out.Set(k, lVal)
			case leftUnchanged:
				out.Set(k, rVal)
			case rightUnchanged:
				out.Set(k, lVal)
			case sameMod:
				out.Set(k, lVal)
			default:
				baseDoc, leftDoc, rightDoc, documents := nestedMergeDocuments(bVal, lVal, rVal)
				if !documents {
					return nil, true
				}
				merged, conflict := mergeBSONDoc(baseDoc, leftDoc, rightDoc)
				if conflict {
					return nil, true
				}
				out.Set(k, merged)
			}
		case bOK && !lOK && !rOK:
		}
	}
	return out, false
}

func getOrNil(doc *types.Document, key string) (any, bool) {
	if doc == nil || !doc.Has(key) {
		return nil, false
	}
	v, err := doc.Get(key)
	if err != nil {
		return nil, false
	}
	return v, true
}

func mergeDocumentKeys(documents ...*types.Document) []string {
	uniqueKeys := make(map[string]struct{})
	for _, document := range documents {
		for _, key := range document.Keys() {
			uniqueKeys[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(uniqueKeys))
	for key := range uniqueKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nestedMergeDocuments(base, left, right any) (*types.Document, *types.Document, *types.Document, bool) {
	baseDoc, baseOK := base.(*types.Document)
	leftDoc, leftOK := left.(*types.Document)
	rightDoc, rightOK := right.(*types.Document)
	return baseDoc, leftDoc, rightDoc, baseOK && leftOK && rightOK
}
