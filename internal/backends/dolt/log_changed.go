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
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// changedFieldPaths returns the set of JSON-path field locations that differ
// between a document's pre-image (docA) and post-image (docB). Either side may
// be nil: a nil docA means the document was added (every field in docB counts
// as changed), a nil docB means it was removed (every field in docA counts).
// For a modification, the changed paths are the field-level diff.
//
// Paths use the FieldDiff form rooted at "$": "$.status", "$.a.b", "$.arr[0]".
// This unifies all three change kinds: diffDocumentPaths(empty, docB) reports
// every field of an added document as added, diffDocumentPaths(docA, empty)
// every field of a removed one as removed.
func changedFieldPaths(docA, docB *types.Document) (map[string]struct{}, error) {
	if docA == nil {
		docA = must.NotFail(types.NewDocument())
	}
	if docB == nil {
		docB = must.NotFail(types.NewDocument())
	}
	fds, err := diffDocumentPaths("$", docA, docB)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(fds))
	for _, fd := range fds {
		set[fd.Path] = struct{}{}
	}
	return set, nil
}

// changedSetMatches reports whether a {field: {$changed: true}} qualifier on
// fieldKey is satisfied by the changed-path set. fieldKey is a dotted field
// path (e.g. "status" or "shipping.carrier"); it matches when a changed path is
// the field itself, an ancestor of it (a wholesale change to an enclosing
// object/array also changes the nested field), or a descendant of it (a change
// nested under the field counts as the field changing).
func changedSetMatches(set map[string]struct{}, fieldKey string) bool {
	q := "$." + fieldKey
	for p := range set {
		if p == q || pathIsAncestor(p, q) || pathIsAncestor(q, p) {
			return true
		}
	}
	return false
}

// pathIsAncestor reports whether path x is a strict ancestor of path y, i.e. y
// continues x with a field ("x.f") or array element ("x[0]") component.
func pathIsAncestor(x, y string) bool {
	return strings.HasPrefix(y, x+".") || strings.HasPrefix(y, x+"[")
}

// matchFilterUsesChanged reports whether a $match query contains any
// {field: {$changed: ...}} qualifier, recursively (including under
// $and/$or/$nor). Callers use this to choose the $changed-aware evaluator over
// the plain FilterDocument fast path.
func matchFilterUsesChanged(filter *types.Document) bool {
	for _, k := range filter.Keys() {
		v, _ := filter.Get(k)
		switch k {
		case "$and", "$or", "$nor":
			if arr, ok := v.(*types.Array); ok {
				for i := 0; i < arr.Len(); i++ {
					e, _ := arr.Get(i)
					if sub, ok := e.(*types.Document); ok && matchFilterUsesChanged(sub) {
						return true
					}
				}
			}
		default:
			if spec, ok := v.(*types.Document); ok && spec.Has("$changed") {
				return true
			}
		}
	}
	return false
}

// evalMatchAgainstImage evaluates a $match query against a single document image
// plus the set of field paths that changed for this document. It handles the
// boolean operators $and/$or/$nor itself and delegates value clauses (and the
// non-$changed remainder of a field spec) to the real find() matcher, so value
// semantics are identical to find(). A {field: {$changed: true}} clause is
// satisfied per changedSetMatches.
//
// The $changed result is a property of the pre<->post pair, so it is the same
// regardless of which image this is evaluated against; callers OR the result
// over the pre- and post-images to keep the existing match-either-image rule.
func evalMatchAgainstImage(image, filter *types.Document, changed map[string]struct{}) (bool, error) {
	result := true
	var residuePairs []any

	for _, k := range filter.Keys() {
		v, _ := filter.Get(k)

		switch k {
		case "$and", "$or", "$nor":
			arr, ok := v.(*types.Array)
			if !ok {
				return false, fmt.Errorf("%s requires an array", k)
			}
			ok, err := evalBoolArray(k, arr, image, changed)
			if err != nil {
				return false, err
			}
			if !ok {
				result = false
			}

		default:
			spec, isDoc := v.(*types.Document)
			if isDoc && spec.Has("$changed") {
				cv, _ := spec.Get("$changed")
				if b, ok := cv.(bool); !ok || !b {
					return false, fmt.Errorf("$changed for field %q must be true", k)
				}
				if !changedSetMatches(changed, k) {
					result = false
				}
				// Any remaining operators on the same field still apply to the
				// image as ordinary value conditions.
				if rem := docWithoutKey(spec, "$changed"); rem != nil {
					residuePairs = append(residuePairs, k, rem)
				}
			} else {
				residuePairs = append(residuePairs, k, v)
			}
		}
	}

	if !result {
		return false, nil
	}
	if len(residuePairs) > 0 {
		residue, err := types.NewDocument(residuePairs...)
		if err != nil {
			return false, err
		}
		ok, err := backends.MatchPartialFilter(image, residue)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// evalBoolArray evaluates a $and/$or/$nor array of sub-filters.
func evalBoolArray(op string, arr *types.Array, image *types.Document, changed map[string]struct{}) (bool, error) {
	anyMatch := false
	allMatch := true
	for i := 0; i < arr.Len(); i++ {
		e, _ := arr.Get(i)
		sub, ok := e.(*types.Document)
		if !ok {
			return false, fmt.Errorf("%s array elements must be documents", op)
		}
		m, err := evalMatchAgainstImage(image, sub, changed)
		if err != nil {
			return false, err
		}
		anyMatch = anyMatch || m
		allMatch = allMatch && m
	}
	switch op {
	case "$and":
		return allMatch, nil
	case "$or":
		return anyMatch, nil
	default: // $nor
		return !anyMatch, nil
	}
}

// docWithoutKey returns a copy of d with omit removed, or nil if nothing remains.
func docWithoutKey(d *types.Document, omit string) *types.Document {
	var pairs []any
	for _, k := range d.Keys() {
		if k == omit {
			continue
		}
		v, _ := d.Get(k)
		pairs = append(pairs, k, v)
	}
	if len(pairs) == 0 {
		return nil
	}
	return must.NotFail(types.NewDocument(pairs...))
}
