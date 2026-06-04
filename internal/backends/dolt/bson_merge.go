// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package dolt

import (
	"reflect"

	"github.com/dolthub/dumbodb/internal/types"
)

// mergeBSONDoc performs a field-level three-way merge of base, left,
// and right types.Document values. Non-overlapping field changes are
// merged automatically; overlapping modifications produce a conflict.
// The boolean return is true when a conflict was detected; the
// returned document in that case is unspecified.
//
// Field-level merge rules:
//
//   - field present only in left (not in base, not in right): added by
//     left; result has left's value.
//   - field present only in right (not in base, not in left): added
//     by right; result has right's value.
//   - field present in left and right but not in base: both added.
//     Take it if left_v equals right_v; otherwise conflict.
//   - field present in base and left but not in right: right
//     deleted; result omits the field.
//   - field present in base and right but not in left: left deleted;
//     result omits the field.
//   - field present in all three:
//     -- left_v == base_v: right modified; result has right_v.
//     -- right_v == base_v: left modified; result has left_v.
//     -- left_v == right_v: both modified the same way; result has
//        left_v.
//     -- else: conflicting modifications.
//
// Field equality uses reflect.DeepEqual on the values returned by
// types.Document.Get, which is sufficient for the scalar and shallow-
// container cases the bson-a merge path handles. Deeply nested
// container conflicts fall back to the dolt prolly three-way differ
// at a higher level when the field-level test reports conflict.
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
			// right deleted: drop the field.
		case bOK && rOK && !lOK:
			// left deleted: drop the field.
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
			// both deleted (or never set on either side); drop.
		}
	}
	return out, false
}

// getOrNil reads a key from doc, returning the value and a found flag.
// The Get error path (key absent) is collapsed into ok=false; deeper
// errors (which the in-memory document type does not produce) would
// surface as ok=false plus a zero value, matching the merge logic's
// expectation.
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
