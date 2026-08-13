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

	"github.com/dolthub/dumbodb/internal/types"
)

// mergeBSONDoc performs a field-level three-way merge. Returns (nil,
// true) on a conflict. Equality is reflect.DeepEqual on Get-returned
// values, sufficient for scalar and shallow-container fields; deeper
// container conflicts fall back to the prolly three-way differ.
func mergeBSONDoc(base, left, right *types.Document) (*types.Document, bool) {
	keys := make(map[string]struct{})
	for _, k := range base.Keys() {
		keys[k] = struct{}{}
	}
	for _, k := range left.Keys() {
		keys[k] = struct{}{}
	}
	for _, k := range right.Keys() {
		keys[k] = struct{}{}
	}
	out := types.MakeDocument(len(keys))
	for k := range keys {
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
				return nil, true
			}
		case bOK && !lOK && !rOK:
		}
	}
	return out, false
}

func getOrNil(doc *types.Document, key string) (any, bool) {
	if !doc.Has(key) {
		return nil, false
	}
	v, err := doc.Get(key)
	if err != nil {
		return nil, false
	}
	return v, true
}
