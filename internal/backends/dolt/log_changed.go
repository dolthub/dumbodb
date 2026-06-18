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
	"strings"

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
